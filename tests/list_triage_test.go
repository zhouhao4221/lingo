package tests

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// seedTriageRow 插入一张合成工单：提交人 user / 姓名 name，triage 非零时为待确认并把 triage_since 设为该时间。
func seedTriageRow(t *testing.T, db *sql.DB, project, repo, user, name, typ, title, status string, triage time.Time) int64 {
	t.Helper()
	var since any
	if !triage.IsZero() {
		since = triage
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO lingo_tickets (user_id, user_name, project_id, repo_id, type, title, body, status, triage_since)
		SELECT $1, $2, p.id, r.id, $5, $6, $6, $7, $8
		FROM lingo_projects p JOIN lingo_repos r ON r.project_id = p.id WHERE p.key = $3 AND r.name = $4 RETURNING id`,
		user, name, project, repo, typ, title, status, since).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// collectTriage 按 Cursor 翻完全部页，返回 ID 序列与页数；每页 Total 须一致。
func collectTriage(t *testing.T, l *lingo.Lingo, u lingo.User, f lingo.TicketListFilter) ([]int64, int, int) {
	t.Helper()
	f.Sort = lingo.TicketSortTriageSince
	var ids []int64
	total, pages := -1, 0
	for {
		p, err := l.ListTickets(context.Background(), u, f)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if total >= 0 && p.Total != total {
			t.Fatalf("page %d: total %d, first page %d", pages, p.Total, total)
		}
		total = p.Total
		if p.NextBefore != 0 {
			t.Fatalf("page %d: next_before %d with triage sort", pages, p.NextBefore)
		}
		ids = append(ids, pageIDs(p)...)
		pages++
		if p.NextCursor == "" {
			return ids, total, pages
		}
		f.Cursor = p.NextCursor
		if pages > 100 {
			t.Fatal("too many pages")
		}
	}
}

// 评审第 1 轮：走真实生命周期——CreateTicket 进入待确认后出现在 sort=triage_since 中；Answer / Reject / Merge / Forward 离开后
// 不再出现、Total 同步减少、TriageSince 已清空。
func TestListTicketsTriageLifecycle(t *testing.T) {
	l, _, _ := newTicketLingo(t, &giteaServer{templates: "[]"}, nil, nil) // 不自动转出：新工单进入待确认
	ctx := context.Background()
	create := func(content string) lingo.Ticket {
		t.Helper()
		tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketFeature, Content: content})
		if err != nil || tk.Status != "pending_triage" || tk.TriageSince == nil {
			t.Fatalf("create %s: %+v %v", content, tk, err)
		}
		return tk
	}
	target, ans, rej, mrg, fwd := create("合并目标"), create("待答复"), create("待驳回"), create("待合并"), create("待转出")
	ids, total, _ := collectTriage(t, l, rita, lingo.TicketListFilter{Limit: 2})
	if want := []int64{target.ID, ans.ID, rej.ID, mrg.ID, fwd.ID}; !slices.Equal(ids, want) || total != 5 {
		t.Fatalf("created: %v total %d, want %v", ids, total, want)
	}
	if p, _ := l.ListTickets(ctx, rita, lingo.TicketListFilter{Sort: lingo.TicketSortTriageSince, SubmitterName: alice.Name}); p.Total != 5 {
		t.Errorf("submitter name from CreateTicket: %+v", p)
	}
	remaining := []int64{target.ID, ans.ID, rej.ID, mrg.ID, fwd.ID}
	for _, c := range []struct {
		name string
		id   int64
		do   func() error
	}{
		{"answer", ans.ID, func() error { _, err := l.AnswerTicket(ctx, rita, ans.ID, "已支持"); return err }},
		{"reject", rej.ID, func() error { _, err := l.RejectTicket(ctx, rita, rej.ID, "不做"); return err }},
		{"merge", mrg.ID, func() error { _, err := l.MergeTicket(ctx, rita, mrg.ID, target.ID, "重复"); return err }},
		{"forward", fwd.ID, func() error { _, err := l.ForwardTicket(ctx, rita, fwd.ID, lingo.ForwardOptions{}); return err }},
	} {
		if err := c.do(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		remaining = slices.DeleteFunc(remaining, func(id int64) bool { return id == c.id })
		ids, total, _ := collectTriage(t, l, rita, lingo.TicketListFilter{Limit: 2})
		if !slices.Equal(ids, remaining) || total != len(remaining) {
			t.Errorf("after %s: %v total %d, want %v", c.name, ids, total, remaining)
		}
		got, err := l.GetTicket(ctx, rita, c.id)
		if err != nil || got.Status == "pending_triage" || got.TriageSince != nil {
			t.Errorf("after %s: status %s triage_since %v err %v", c.name, got.Status, got.TriageSince, err)
		}
	}
}

// 待办 #789：待确认时间正序（实际进入待确认的 triage_since，不用提交时间）与提交人姓名筛选；角色边界、组合、游标、默认旧行为。
func TestListTicketsTriageOrderAndSubmitter(t *testing.T) {
	l, db, a := newAudienceLingo(t, nopModel{})
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	tie := base.Add(30*time.Second + 123456*time.Microsecond) // 并列时间（微秒精度）
	// ID 递增，但进入待确认的时间与 ID 顺序不同；提交时间（created_at）都是 now()，不能用来排序
	p1 := seedTriageRow(t, db, "demo", "api", "u1", "张三", "bug", "登录白屏", "pending_triage", at(300))
	p2 := seedTriageRow(t, db, "demo", "web", "u2", "Alice Wang", "bug", "列表报错", "pending_triage", tie)
	p3 := seedTriageRow(t, db, "demo", "api", "u1", "张三丰", "feature", "导出", "pending_triage", tie)
	o4 := seedTriageRow(t, db, "demo", "api", "u2", "alice wang", "bug", "已转研发", "open", time.Time{})
	p5 := seedTriageRow(t, db, "demo", "web", "u3", "李四", "bug", "早的", "pending_triage", at(0))
	p6 := seedTriageRow(t, db, "other", "svc", "u1", "张三", "bug", "其他项目", "pending_triage", at(-600))
	p7 := seedTriageRow(t, db, "demo", "web", "u2", "Alice Wang", "bug", "并列的第三张", "pending_triage", tie)
	r8 := seedTriageRow(t, db, "demo", "api", "u3", "李四", "bug", "已驳回", "rejected", time.Time{})
	p9 := seedTriageRow(t, db, "demo", "api", "u4", "100%_王", "consult", "特殊字符", "pending_triage", at(600))
	// 提交时间与待确认时间相反：若误用 created_at 排序，结果会不同
	if _, err := db.Exec("UPDATE lingo_tickets SET created_at = now() - (id * interval '1 hour')"); err != nil {
		t.Fatal(err)
	}
	a.set("u1", "u2")
	demoRev := lingo.User{ID: "r9", Role: lingo.RoleReviewer, Projects: []string{"demo"}}
	otherAdmin := lingo.User{ID: "root", Role: lingo.RoleAdmin, Projects: []string{"other"}}
	demoAgent := lingo.User{ID: "a1", Role: lingo.RoleAgent, Projects: []string{"demo"}}
	demoOrder := []int64{p5, p2, p3, p7, p1, p9} // 同一 tie 时间按 ID 正序

	// 1. 排序、并列键与游标：各页大小都拼回全集，无重无漏
	for _, size := range []int{1, 2, 3, 4, 6, 50} {
		ids, total, pages := collectTriage(t, l, demoRev, lingo.TicketListFilter{Limit: size})
		if !slices.Equal(ids, demoOrder) || total != 6 || pages != (6+size-1)/size {
			t.Errorf("size %d: %v total %d pages %d, want %v", size, ids, total, pages, demoOrder)
		}
	}
	// 范围为空的 reviewer（Config.Projects 全部）含 other 项目，且最早
	if ids, total, _ := collectTriage(t, l, rita, lingo.TicketListFilter{Limit: 2}); !slices.Equal(ids, append([]int64{p6}, demoOrder...)) || total != 7 {
		t.Errorf("rita: %v %d", ids, total)
	}

	// 2. 角色边界
	for _, c := range []struct {
		name string
		u    lingo.User
		f    lingo.TicketListFilter
		want []int64
	}{
		{"external own", alice, lingo.TicketListFilter{}, []int64{p6, p3, p1}},
		{"external shared", alice, lingo.TicketListFilter{IncludeShared: true}, []int64{p6, p2, p3, p7, p1}},
		{"external shared other name", alice, lingo.TicketListFilter{IncludeShared: true, SubmitterName: "alice"}, []int64{p2, p7}},
		{"external other name not shared", alice, lingo.TicketListFilter{SubmitterName: "alice"}, []int64{}},
		{"external stranger name", alice, lingo.TicketListFilter{IncludeShared: true, SubmitterName: "李四"}, []int64{}},
		{"agent never pending", demoAgent, lingo.TicketListFilter{}, []int64{}},
		{"admin scoped", otherAdmin, lingo.TicketListFilter{}, []int64{p6}},
		{"admin outside project", otherAdmin, lingo.TicketListFilter{Project: "demo"}, []int64{}},
		{"reviewer outside project", demoRev, lingo.TicketListFilter{Project: "other"}, []int64{}},
		{"reviewer mine", rita, lingo.TicketListFilter{Mine: true}, []int64{}},
	} {
		ids, total, _ := collectTriage(t, l, c.u, c.f)
		if !slices.Equal(ids, c.want) && !(len(ids) == 0 && len(c.want) == 0) || total != len(c.want) {
			t.Errorf("%s: %v total %d, want %v", c.name, ids, total, c.want)
		}
	}

	// 3. 姓名与组合筛选（triage 排序）
	for _, c := range []struct {
		name string
		f    lingo.TicketListFilter
		want []int64
	}{
		{"name substring", lingo.TicketListFilter{SubmitterName: "张三"}, []int64{p3, p1}},
		{"name case-insensitive", lingo.TicketListFilter{SubmitterName: " ALICE WANG "}, []int64{p2, p7}},
		{"name like escape", lingo.TicketListFilter{SubmitterName: "%_"}, []int64{p9}},
		{"name no wildcard", lingo.TicketListFilter{SubmitterName: "_"}, []int64{p9}},
		{"name + type", lingo.TicketListFilter{SubmitterName: "张三", Type: lingo.TicketFeature}, []int64{p3}},
		{"name + repo", lingo.TicketListFilter{SubmitterName: "alice", Repo: "demo/web"}, []int64{p2, p7}},
		{"keyword + name", lingo.TicketListFilter{SubmitterName: "alice", Keyword: "并列"}, []int64{p7}},
		{"status intersect", lingo.TicketListFilter{Statuses: []string{"open", "rejected"}}, []int64{}},
		{"status pending", lingo.TicketListFilter{Statuses: []string{"pending_triage", "open"}}, demoOrder},
	} {
		ids, total, _ := collectTriage(t, l, demoRev, lingo.TicketListFilter{SubmitterName: c.f.SubmitterName, Type: c.f.Type, Repo: c.f.Repo,
			Keyword: c.f.Keyword, Statuses: c.f.Statuses, Limit: 1})
		if !slices.Equal(ids, c.want) && !(len(ids) == 0 && len(c.want) == 0) || total != len(c.want) {
			t.Errorf("%s: %v total %d, want %v", c.name, ids, total, c.want)
		}
	}

	// 4. 默认排序（不传 Sort）不变：ID 降序、含非待确认、BeforeID 翻页、NextCursor 为空；姓名筛选同样生效
	p, err := l.ListTickets(ctx, demoRev, lingo.TicketListFilter{})
	if want := []int64{p9, r8, p7, p5, o4, p3, p2, p1}; err != nil || !slices.Equal(pageIDs(p), want) || p.Total != 8 || p.NextCursor != "" || p.NextBefore != 0 {
		t.Errorf("default: %v %+v %v", pageIDs(p), p, err)
	}
	p, err = l.ListTickets(ctx, demoRev, lingo.TicketListFilter{SubmitterName: "alice", Limit: 2})
	if err != nil || !slices.Equal(pageIDs(p), []int64{p7, o4}) || p.Total != 3 || p.NextBefore != o4 || p.NextCursor != "" {
		t.Errorf("default name page 1: %v %+v %v", pageIDs(p), p, err)
	}
	p, err = l.ListTickets(ctx, demoRev, lingo.TicketListFilter{SubmitterName: "alice", Limit: 2, BeforeID: o4})
	if err != nil || !slices.Equal(pageIDs(p), []int64{p2}) || p.NextBefore != 0 {
		t.Errorf("default name page 2: %v %+v %v", pageIDs(p), p, err)
	}

	// 5. 翻页途中：前一页的工单离开待确认、新工单进入待确认，不重复、不漏掉仍符合条件的
	f := lingo.TicketListFilter{Sort: lingo.TicketSortTriageSince, Limit: 2}
	p, _ = l.ListTickets(ctx, demoRev, f) // p5, p2
	if _, err := db.Exec("UPDATE lingo_tickets SET status = 'open', triage_since = NULL WHERE id IN ($1, $2)", p2, p3); err != nil {
		t.Fatal(err)
	}
	late := seedTriageRow(t, db, "demo", "api", "u5", "后来者", "bug", "后进入", "pending_triage", at(9000))
	var rest []int64
	for f.Cursor = p.NextCursor; f.Cursor != ""; f.Cursor = p.NextCursor {
		if p, err = l.ListTickets(ctx, demoRev, f); err != nil {
			t.Fatal(err)
		}
		rest = append(rest, pageIDs(p)...)
	}
	if want := []int64{p7, p1, p9, late}; !slices.Equal(rest, want) {
		t.Errorf("concurrent change: %v, want %v", rest, want)
	}

	// 6. 不合法
	for _, c := range []struct {
		name string
		f    lingo.TicketListFilter
	}{
		{"bad sort", lingo.TicketListFilter{Sort: "created"}},
		{"before with sort", lingo.TicketListFilter{Sort: lingo.TicketSortTriageSince, BeforeID: p1}},
		{"cursor without sort", lingo.TicketListFilter{Cursor: "MTox"}},
		{"bad cursor", lingo.TicketListFilter{Sort: lingo.TicketSortTriageSince, Cursor: "not-a-cursor"}},
		{"cursor time out of range", lingo.TicketListFilter{Sort: lingo.TicketSortTriageSince, Cursor: "LTkyMjMzNzIwMzY4NTQ3NzU4MDg6MQ"}}, // "-9223372036854775808:1"
		{"name too long", lingo.TicketListFilter{SubmitterName: strings.Repeat("名", 101)}},
	} {
		if p, err := l.ListTickets(ctx, demoRev, c.f); !errors.Is(err, lingo.ErrInvalidInput) || len(p.Tickets) != 0 {
			t.Errorf("%s: %v %+v", c.name, err, p)
		}
	}

	// 7. REST
	rc := restClient{t: t, h: l.Handler()}
	var ids []int64
	path := "/tickets?sort=triage_since&limit=2&submitter=" + url.QueryEscape("alice")
	for cursor, n := "", 0; ; n++ {
		q := path
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		code, obj, _ := rc.do("r1", "reviewer", "GET", q, nil)
		if code != 200 || obj["total"] != float64(1) || obj["next_before"] != float64(0) {
			t.Fatalf("rest triage page %d: %d %v", n, code, obj)
		}
		for _, x := range obj["tickets"].([]any) {
			ids = append(ids, int64(x.(map[string]any)["id"].(float64)))
		}
		c, _ := obj["next_cursor"].(string)
		if c == "" {
			break
		}
		cursor = c
	}
	if !slices.Equal(ids, []int64{p7}) { // p2 已在第 5 步离开待确认
		t.Errorf("rest triage: %v", ids)
	}
	code, obj, _ := rc.do("r1", "reviewer", "GET", "/tickets?sort=triage_since&limit=3", nil)
	if code != 200 || obj["total"] != float64(6) || obj["next_cursor"] == nil {
		t.Errorf("rest triage page: %d %v", code, obj)
	}
	code, obj, _ = rc.do("r1", "reviewer", "GET", "/tickets?limit=1", nil)
	if _, has := obj["next_cursor"]; code != 200 || has || obj["next_before"] == float64(0) {
		t.Errorf("rest default keeps next_before only: %d %v", code, obj)
	}
	code, obj, _ = rc.do("u1", "external", "GET", "/tickets?sort=triage_since&submitter="+url.QueryEscape("李四"), nil)
	if code != 200 || obj["total"] != float64(0) {
		t.Errorf("rest external stranger: %d %v", code, obj)
	}
	for _, q := range []string{"?sort=x", "?sort=triage_since&before=1", "?cursor=MTox", "?sort=triage_since&cursor=bad", "?sort=triage_since&cursor=LTkyMjMzNzIwMzY4NTQ3NzU4MDg6MQ", "?sort=triage_since&cursor=OTIyMzM3MjAzNjg1NDc3NTgwNzox", "?submitter=" + strings.Repeat("a", 101)} {
		if code, obj, _ := rc.do("r1", "reviewer", "GET", "/tickets"+q, nil); code != 400 {
			t.Errorf("rest %s: %d %v", q, code, obj)
		}
	}
}
