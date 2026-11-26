package tests

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// seedListRow 在项目 project 的仓库 repo 下插入一张合成工单。
func seedListRow(t *testing.T, db *sql.DB, project, repo, user, typ, title, status string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO lingo_tickets (user_id, user_name, project_id, repo_id, type, title, body, status, triage_since)
		SELECT $1, $1, p.id, r.id, $4, $5, $5 || ' 的详细描述', $6, CASE WHEN $6 = 'pending_triage' THEN now() END
		FROM lingo_projects p JOIN lingo_repos r ON r.project_id = p.id WHERE p.key = $2 AND r.name = $3 RETURNING id`,
		user, project, repo, typ, title, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func pageIDs(p lingo.TicketPage) []int64 {
	out := []int64{}
	for _, t := range p.Tickets {
		out = append(out, t.ID)
	}
	return out
}

// REQ-010：反馈管理列表按既有可见范围返回，跨项目隔离，筛选与分页正确，无权限时不泄露。
func TestListTickets(t *testing.T) {
	l, db, a := newAudienceLingo(t, nopModel{})
	ctx := context.Background()
	t1 := seedListRow(t, db, "demo", "api", "u1", "bug", "登录白屏", "pending_triage")
	t2 := seedListRow(t, db, "demo", "web", "u2", "bug", "登录后列表报错", "open")
	t3 := seedListRow(t, db, "demo", "api", "u1", "feature", "导出 Excel", "open")
	t4 := seedListRow(t, db, "other", "svc", "u3", "bug", "同步失败", "open")
	t5 := seedListRow(t, db, "other", "svc", "u1", "consult", "怎么开票", "submitted")
	t6 := seedListRow(t, db, "demo", "api", "u2", "bug", "已驳回的问题", "rejected")
	t7 := seedListRow(t, db, "demo", "web", "r1", "feature", "暗色模式", "pending_triage")
	// 配置中已删除的项目：数据库里仍有，User.Projects 为空的调用者不应看到
	if _, err := db.Exec(`INSERT INTO lingo_projects (key, name) VALUES ('legacy', 'Legacy');
		INSERT INTO lingo_repos (project_id, owner, name) SELECT id, 'legacy', 'old' FROM lingo_projects WHERE key = 'legacy'`); err != nil {
		t.Fatal(err)
	}
	t8 := seedListRow(t, db, "legacy", "old", "u1", "bug", "旧项目的工单", "open")
	a.set("u1", "u2")

	demoRev := lingo.User{ID: "r9", Name: "Rev", Role: lingo.RoleReviewer, Projects: []string{"demo"}}
	otherAdmin := lingo.User{ID: "root", Name: "Root", Role: lingo.RoleAdmin, Projects: []string{"other"}}
	demoAgent := lingo.User{ID: "a1", Name: "dev-agent", Role: lingo.RoleAgent, Projects: []string{"demo"}}
	for _, c := range []struct {
		name string
		u    lingo.User
		f    lingo.TicketListFilter
		want []int64
	}{
		{"external default own only", alice, lingo.TicketListFilter{}, []int64{t5, t3, t1}},
		{"external mine", alice, lingo.TicketListFilter{Mine: true, IncludeShared: true}, []int64{t5, t3, t1}},
		{"external shared", alice, lingo.TicketListFilter{IncludeShared: true}, []int64{t6, t5, t3, t2, t1}},
		{"external shared project", alice, lingo.TicketListFilter{IncludeShared: true, Project: "other"}, []int64{t5}},
		{"external unknown project", alice, lingo.TicketListFilter{Project: "legacy"}, []int64{}},
		{"external no audience", bob, lingo.TicketListFilter{IncludeShared: true}, []int64{t6, t2}},
		{"agent forwarded only", agent, lingo.TicketListFilter{}, []int64{t5, t4, t3, t2}},
		{"agent project scope", demoAgent, lingo.TicketListFilter{}, []int64{t3, t2}},
		{"agent mine", agent, lingo.TicketListFilter{Mine: true}, []int64{}},
		{"reviewer declared projects", rita, lingo.TicketListFilter{}, []int64{t7, t6, t5, t4, t3, t2, t1}},
		{"reviewer mine", rita, lingo.TicketListFilter{Mine: true}, []int64{t7}},
		{"reviewer scoped", demoRev, lingo.TicketListFilter{}, []int64{t7, t6, t3, t2, t1}},
		{"reviewer outside project", demoRev, lingo.TicketListFilter{Project: "other"}, []int64{}},
		{"reviewer legacy project", rita, lingo.TicketListFilter{Project: "legacy"}, []int64{}},
		{"admin scoped", otherAdmin, lingo.TicketListFilter{}, []int64{t5, t4}},
		{"admin outside project", otherAdmin, lingo.TicketListFilter{Project: "demo"}, []int64{}},
		{"status", rita, lingo.TicketListFilter{Statuses: []string{"open"}}, []int64{t4, t3, t2}},
		{"statuses", rita, lingo.TicketListFilter{Statuses: []string{"pending_triage", "rejected"}}, []int64{t7, t6, t1}},
		{"type", rita, lingo.TicketListFilter{Type: lingo.TicketFeature}, []int64{t7, t3}},
		{"repo", rita, lingo.TicketListFilter{Repo: "Demo/Web"}, []int64{t7, t2}},
		{"keyword", rita, lingo.TicketListFilter{Keyword: " 登录  "}, []int64{t2, t1}},
		{"keywords all", rita, lingo.TicketListFilter{Keyword: "登录 列表"}, []int64{t2}},
		{"keyword like escape", rita, lingo.TicketListFilter{Keyword: "%"}, []int64{}},
		{"combined", rita, lingo.TicketListFilter{Project: "demo", Type: lingo.TicketBug, Statuses: []string{"open", "rejected"}}, []int64{t6, t2}},
		{"external filter stays own", alice, lingo.TicketListFilter{Type: lingo.TicketBug}, []int64{t1}},
	} {
		p, err := l.ListTickets(ctx, c.u, c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := pageIDs(p); !slices.Equal(got, c.want) || p.Total != len(c.want) || p.NextBefore != 0 {
			t.Errorf("%s: ids %v total %d next %d, want %v", c.name, got, p.Total, p.NextBefore, c.want)
		}
	}
	_ = t8

	// Mine 标记：同事的为 false
	p, _ := l.ListTickets(ctx, alice, lingo.TicketListFilter{IncludeShared: true, Project: "demo"})
	for _, tk := range p.Tickets {
		if tk.Mine != (tk.UserID == "u1") {
			t.Errorf("mine flag %d = %v", tk.ID, tk.Mine)
		}
	}
	// 时间筛选：把 t1 提交时间改到过去
	if _, err := db.Exec("UPDATE lingo_tickets SET created_at = now() - interval '10 days' WHERE id = $1", t1); err != nil {
		t.Fatal(err)
	}
	cut := time.Now().Add(-24 * time.Hour)
	if p, err := l.ListTickets(ctx, alice, lingo.TicketListFilter{To: cut}); err != nil || !slices.Equal(pageIDs(p), []int64{t1}) {
		t.Errorf("to: %v %v", pageIDs(p), err)
	}
	if p, err := l.ListTickets(ctx, alice, lingo.TicketListFilter{From: cut, TimeField: "created"}); err != nil || !slices.Equal(pageIDs(p), []int64{t5, t3}) {
		t.Errorf("from: %v %v", pageIDs(p), err)
	}

	// 分页：7 张，每页 3
	var all []int64
	before, pages := int64(0), 0
	for {
		p, err := l.ListTickets(ctx, rita, lingo.TicketListFilter{BeforeID: before, Limit: 3})
		if err != nil || p.Total != 7 {
			t.Fatalf("page %d: %+v %v", pages, p, err)
		}
		all = append(all, pageIDs(p)...)
		pages++
		if p.NextBefore == 0 {
			break
		}
		before = p.NextBefore
	}
	if !slices.Equal(all, []int64{t7, t6, t5, t4, t3, t2, t1}) || pages != 3 {
		t.Errorf("paged %v in %d pages", all, pages)
	}
	if p, _ := l.ListTickets(ctx, rita, lingo.TicketListFilter{Limit: 7}); p.NextBefore != 0 || len(p.Tickets) != 7 {
		t.Errorf("exact page: next %d len %d", p.NextBefore, len(p.Tickets))
	}

	// 无权限与非法条件
	for _, c := range []struct {
		name string
		u    lingo.User
		f    lingo.TicketListFilter
		want error
	}{
		{"unknown role", lingo.User{ID: "x", Role: "guest"}, lingo.TicketListFilter{}, lingo.ErrForbidden},
		{"external without id", lingo.User{Role: lingo.RoleExternal}, lingo.TicketListFilter{IncludeShared: true}, lingo.ErrForbidden},
		{"bad type", rita, lingo.TicketListFilter{Type: "x"}, lingo.ErrInvalidInput},
		{"bad status", rita, lingo.TicketListFilter{Statuses: []string{"done"}}, lingo.ErrInvalidInput},
		{"bad time field", rita, lingo.TicketListFilter{TimeField: "x"}, lingo.ErrInvalidInput},
		{"bad repo", rita, lingo.TicketListFilter{Repo: "noslash"}, lingo.ErrInvalidInput},
		{"too many keywords", rita, lingo.TicketListFilter{Keyword: "1 2 3 4 5 6 7 8 9 10 11"}, lingo.ErrInvalidInput},
	} {
		if p, err := l.ListTickets(ctx, c.u, c.f); !errors.Is(err, c.want) || len(p.Tickets) != 0 {
			t.Errorf("%s: %v %+v", c.name, err, p)
		}
	}

	// 同事名单回调出错：按未放宽处理
	a.mu.Lock()
	a.fail = true
	a.mu.Unlock()
	if p, err := l.ListTickets(ctx, alice, lingo.TicketListFilter{IncludeShared: true}); err != nil || !slices.Equal(pageIDs(p), []int64{t5, t3, t1}) {
		t.Errorf("audience error: %v %v", pageIDs(p), err)
	}
	a.mu.Lock()
	a.fail = false
	a.calls = nil
	a.mu.Unlock()
	// 默认（不含同事）不调用回调；非 external 也不调用
	_, _ = l.ListTickets(ctx, alice, lingo.TicketListFilter{})
	_, _ = l.ListTickets(ctx, rita, lingo.TicketListFilter{IncludeShared: true})
	a.mu.Lock()
	if len(a.calls) != 0 {
		t.Errorf("audience called: %v", a.calls)
	}
	a.mu.Unlock()

	// REST：GET /tickets
	rc := restClient{t: t, h: l.Handler()}
	if code, _, _ := rc.do("", "", "GET", "/tickets", nil); code != 401 {
		t.Errorf("anonymous: %d", code)
	}
	code, obj, _ := rc.do("u1", "external", "GET", "/tickets", nil)
	if code != 200 || obj["total"] != float64(3) || len(obj["tickets"].([]any)) != 3 || obj["next_before"] != float64(0) {
		t.Errorf("rest external: %d %v", code, obj)
	}
	code, obj, _ = rc.do("u1", "external", "GET", "/tickets?shared=true&project=demo", nil)
	if code != 200 || obj["total"] != float64(4) {
		t.Errorf("rest shared: %d %v", code, obj)
	}
	code, obj, _ = rc.do("r1", "reviewer", "GET", "/tickets?status=open,rejected&type=bug&q=%E7%99%BB%E5%BD%95&limit=1", nil)
	if code != 200 || obj["total"] != float64(1) || obj["tickets"].([]any)[0].(map[string]any)["id"] != float64(t2) {
		t.Errorf("rest filters: %d %v", code, obj)
	}
	code, obj, _ = rc.do("r1", "reviewer", "GET", "/tickets?limit=2", nil)
	if code != 200 || obj["total"] != float64(7) || obj["next_before"] != float64(t6) {
		t.Errorf("rest page 1: %d %v", code, obj)
	}
	code, obj, _ = rc.do("r1", "reviewer", "GET", fmt.Sprintf("/tickets?limit=2&before=%d", t6), nil)
	if ts := obj["tickets"].([]any); code != 200 || len(ts) != 2 || ts[0].(map[string]any)["id"] != float64(t5) {
		t.Errorf("rest page 2: %d %v", code, obj)
	}
	code, obj, _ = rc.do("a1", "agent", "GET", "/tickets?mine=true", nil)
	if code != 200 || obj["total"] != float64(0) || len(obj["tickets"].([]any)) != 0 {
		t.Errorf("rest agent mine: %d %v", code, obj)
	}
	code, obj, _ = rc.do("r1", "reviewer", "GET", "/tickets?project=legacy", nil)
	if code != 200 || obj["total"] != float64(0) {
		t.Errorf("rest legacy: %d %v", code, obj)
	}
	for _, q := range []string{"?mine=yes", "?type=x", "?status=done", "?before=abc", "?from=yesterday", "?repo=x", "?time_field=x"} {
		if code, obj, _ := rc.do("r1", "reviewer", "GET", "/tickets"+q, nil); code != 400 {
			t.Errorf("rest %s: %d %v", q, code, obj)
		}
	}
	if code, obj, _ := rc.do("x", "guest", "GET", "/tickets", nil); code != 403 {
		t.Errorf("rest guest: %d %v", code, obj)
	}
}
