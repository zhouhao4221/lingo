package tests

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// commentGitea 是只读的模拟 Gitea：返回机器人账号与各 Issue 的评论（合成数据），记录被读取的 Issue。
type commentGitea struct {
	mu       sync.Mutex
	comments map[string][]map[string]any // "owner/name#n" → 评论
	reads    []string
	writes   int
}

func (g *commentGitea) set(issue string, cs ...map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.comments[issue] = cs
}

func gcomment(id int64, login, body string, at time.Time) map[string]any {
	return map[string]any{"id": id, "body": body, "user": map[string]any{"login": login},
		"created_at": at.Format(time.RFC3339), "updated_at": at.Format(time.RFC3339)}
}

func (g *commentGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	w.Header().Set("Content-Type", "application/json")
	if r.Method != "GET" {
		g.writes++
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if p == "/user" {
		_, _ = w.Write([]byte(`{"id":1,"login":"lingo-bot"}`))
		return
	}
	if s := strings.Split(strings.Trim(p, "/"), "/"); len(s) == 6 && s[0] == "repos" && s[3] == "issues" && s[5] == "comments" {
		key := s[1] + "/" + s[2] + "#" + s[4]
		g.reads = append(g.reads, key)
		cs, ok := g.comments[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(cs)
		return
	}
	_, _ = w.Write([]byte("[]"))
}

func newResyncLingo(t *testing.T, db *sql.DB, g http.Handler, mode lingo.ReplyMode, n lingo.Notifier) *lingo.Lingo {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{}, SkipSelfCheck: true, ReconcileInterval: -1, Notifier: n,
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s", ReplyMode: mode},
		Projects: []lingo.Project{
			{Key: "demo", Name: "Demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}, {Owner: "demo", Name: "web"}}},
			{Key: "other", Name: "Other", Repos: []lingo.Repo{{Owner: "other", Name: "svc"}}}},
		Authenticate: headerUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.ListTriage(context.Background(), rita, lingo.TriageFilter{}); err != nil { // 同步项目入库
		t.Fatal(err)
	}
	return l
}

func seedIssueTicket(t *testing.T, db *sql.DB, project, repo, user, status string, issue int64) int64 {
	t.Helper()
	id := seedListRow(t, db, project, repo, user, "bug", "合成工单 "+user, status)
	if issue > 0 {
		if _, err := db.Exec("UPDATE lingo_tickets SET gitea_issue_number = $1 WHERE id = $2", issue, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func commentBodies(cs []lingo.Comment) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

// REQ-011：ReplyMarked 时保存为内部的历史评论，切换为 ReplyAll 后经 ResyncTicketComments 回补为对外；
// 只读该工单的主 Issue、默认不通知、幂等，授权严格限于项目内的 reviewer / admin。
func TestResyncTicketComments(t *testing.T) {
	db := testDB(t, true)
	g := &commentGitea{comments: map[string][]map[string]any{}}
	marked := newResyncLingo(t, db, g, "", nil)
	ctx := context.Background()
	at := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)

	t1 := seedIssueTicket(t, db, "demo", "api", "u1", "in_progress", 12)
	t2 := seedIssueTicket(t, db, "demo", "web", "u2", "open", 13)
	t3 := seedIssueTicket(t, db, "other", "svc", "u3", "open", 3)
	pending := seedIssueTicket(t, db, "demo", "api", "u1", "pending_triage", 0)
	gone := seedIssueTicket(t, db, "demo", "api", "u1", "open", 99)
	g.set("demo/api#12",
		gcomment(101, "dev", "内部讨论：怀疑是缓存", at),
		gcomment(102, "dev", "/reply 已定位，明天修复", at),
		gcomment(103, "lingo-bot", "【灵工转发】用户补充：还是白屏", at))
	g.set("demo/web#13", gcomment(201, "dev", "u2 工单的内部评论", at))
	g.set("other/svc#3", gcomment(301, "dev", "other 项目的内部评论", at))

	// 1. ReplyMarked 下首次同步：只有 /reply 对外，机器人评论跳过
	r, err := marked.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.TicketID != t1 || r.Repo != "demo/api" || r.Issue != 12 || r.Fetched != 3 || r.Bot != 1 || r.Added != 2 ||
		r.Published != 1 || r.Notified {
		t.Fatalf("marked resync = %+v", r)
	}
	if cs, _ := marked.ListTicketComments(ctx, alice, t1); len(cs) != 1 || cs[0].Body != "已定位，明天修复" {
		t.Fatalf("submitter sees %v", commentBodies(cs))
	}
	// 2. 切换为 ReplyAll（同库新实例），回补历史评论：内部 → 对外，默认不通知
	all := newResyncLingo(t, db, g, lingo.ReplyAll, nil)
	evBefore := countRows(t, db, "SELECT count(*) FROM lingo_events WHERE kind = 'ticket.comment'")
	r, err = all.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Fetched != 3 || r.Bot != 1 || r.Added != 0 || r.Updated != 1 || r.Unchanged != 1 || r.Published != 1 || r.Unpublished != 0 || r.Notified {
		t.Fatalf("all resync = %+v", r)
	}
	if cs, _ := all.ListTicketComments(ctx, alice, t1); len(cs) != 2 || cs[0].Body != "内部讨论：怀疑是缓存" {
		t.Fatalf("after backfill submitter sees %v", commentBodies(cs))
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_events WHERE kind = 'ticket.comment'"); n != evBefore {
		t.Errorf("silent backfill emitted events: %d → %d", evBefore, n)
	}
	// 3. 幂等：再次执行全部 Unchanged，不新增审计
	hist := countRows(t, db, "SELECT count(*) FROM lingo_ticket_events WHERE ticket_id = $1", t1)
	r, err = all.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{})
	if err != nil || r.Unchanged != 2 || r.Added+r.Updated+r.Published+r.Unpublished != 0 {
		t.Errorf("repeat = %+v %v", r, err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_ticket_events WHERE ticket_id = $1", t1); n != hist {
		t.Errorf("repeat added history: %d → %d", hist, n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_ticket_comments WHERE ticket_id = $1 AND source = 'gitea'", t1); n != 2 {
		t.Errorf("gitea comment rows = %d (bot comment must not be imported, no duplicates)", n)
	}
	// 4. 编辑：Gitea 上更新后的正文写回；灵工已记为删除的不恢复
	g.set("demo/api#12",
		gcomment(101, "dev", "内部讨论：确认是缓存", at.Add(time.Hour)),
		gcomment(102, "dev", "/reply 已定位，明天修复", at),
		gcomment(103, "lingo-bot", "【灵工转发】用户补充：还是白屏", at))
	if _, err := db.Exec("UPDATE lingo_ticket_comments SET deleted_at = now() WHERE gitea_comment_id = 102"); err != nil {
		t.Fatal(err)
	}
	r, err = all.ResyncTicketComments(ctx, admin, t1, lingo.CommentResyncOptions{})
	if err != nil || r.Updated != 1 || r.Unchanged != 1 {
		t.Errorf("edit = %+v %v", r, err)
	}
	if cs, _ := all.ListTicketComments(ctx, alice, t1); len(cs) != 1 || cs[0].Body != "内部讨论：确认是缓存" {
		t.Errorf("after edit / delete submitter sees %v", commentBodies(cs))
	}
	// 5. 隔离：只读取该工单的主 Issue，其他工单的评论不被写入
	g.mu.Lock()
	for _, k := range g.reads {
		if k != "demo/api#12" {
			t.Errorf("read other issue %s", k)
		}
	}
	writes := g.writes
	g.mu.Unlock()
	if writes != 0 {
		t.Errorf("resync wrote to Gitea %d times", writes)
	}
	for _, id := range []int64{t2, t3} {
		if n := countRows(t, db, "SELECT count(*) FROM lingo_ticket_comments WHERE ticket_id = $1", id); n != 0 {
			t.Errorf("ticket %d got %d comments", id, n)
		}
	}
	// 5b. 未写入的情况不计变化、不留痕、可重复：灵工已删除（Gitea 正文不同 / 原本对外在 marked 下改判内部）、乱序旧版本
	t5 := seedIssueTicket(t, db, "demo", "api", "u1", "open", 20)
	g.set("demo/api#20", gcomment(501, "dev", "/reply 对外回复", at), gcomment(502, "dev", "内部讨论", at))
	if r, err := all.ResyncTicketComments(ctx, rita, t5, lingo.CommentResyncOptions{}); err != nil || r.Added != 2 || r.Published != 2 {
		t.Fatalf("t5 seed: %+v %v", r, err)
	}
	if _, err := db.Exec("UPDATE lingo_ticket_comments SET deleted_at = now() WHERE gitea_comment_id IN (501, 502)"); err != nil {
		t.Fatal(err)
	}
	g.set("demo/api#20", gcomment(501, "dev", "/reply 对外回复（已改）", at.Add(time.Hour)), gcomment(502, "dev", "内部讨论（已改）", at.Add(time.Hour)))
	audit := func() int {
		return countRows(t, db, "SELECT count(*) FROM lingo_ticket_events WHERE ticket_id = $1", t5)
	}
	feed := func() int { return countRows(t, db, "SELECT count(*) FROM lingo_events WHERE ticket_id = $1", t5) }
	a0, f0 := audit(), feed()
	for _, c := range []struct {
		name string
		l    *lingo.Lingo
	}{{"deleted, body differs (all)", all}, {"deleted, body differs (all) again", all}, {"deleted public → internal (marked)", marked}, {"deleted public → internal (marked) again", marked}} {
		r, err := c.l.ResyncTicketComments(ctx, rita, t5, lingo.CommentResyncOptions{Notify: true})
		if err != nil || r.Unchanged != 2 || r.Added+r.Updated+r.Published+r.Unpublished != 0 || r.Notified {
			t.Errorf("%s: %+v %v", c.name, r, err)
		}
	}
	if a, f := audit(), feed(); a != a0 || f != f0 {
		t.Errorf("unsaved changes left traces: audit %d → %d, feed %d → %d", a0, a, f0, f)
	}
	if cs, _ := all.ListTicketComments(ctx, alice, t5); len(cs) != 0 {
		t.Errorf("deleted comments visible: %v", commentBodies(cs))
	}
	// 乱序旧版本：Gitea 返回的 updated_at 早于已保存版本时不覆盖
	g.set("demo/api#20", gcomment(503, "dev", "最新正文", at.Add(2*time.Hour)))
	if r, err := all.ResyncTicketComments(ctx, rita, t5, lingo.CommentResyncOptions{}); err != nil || r.Added != 1 {
		t.Fatalf("t5 new: %+v %v", r, err)
	}
	a0 = audit()
	g.set("demo/api#20", gcomment(503, "dev", "旧正文", at.Add(time.Hour)))
	for i := range 2 {
		if r, err := all.ResyncTicketComments(ctx, rita, t5, lingo.CommentResyncOptions{}); err != nil || r.Unchanged != 1 || r.Updated != 0 {
			t.Errorf("out-of-order %d: %+v %v", i, r, err)
		}
	}
	if cs, _ := all.ListTicketComments(ctx, alice, t5); len(cs) != 1 || cs[0].Body != "最新正文" || audit() != a0 {
		t.Errorf("out-of-order overwrote: %v audit %d → %d", commentBodies(cs), a0, audit())
	}

	// 6. 授权：external / agent 403（先于查找），越项目与不存在 404，无关联 Issue / Issue 已删除 409
	demoRev := lingo.User{ID: "r9", Name: "Rev", Role: lingo.RoleReviewer, Projects: []string{"demo"}}
	otherAdmin := lingo.User{ID: "root2", Name: "Root", Role: lingo.RoleAdmin, Projects: []string{"other"}}
	for _, c := range []struct {
		name string
		u    lingo.User
		id   int64
		want error
	}{
		{"submitter", alice, t1, lingo.ErrForbidden},
		{"agent", agent, t1, lingo.ErrForbidden},
		{"external probing missing", alice, 999999, lingo.ErrForbidden},
		{"unknown role", lingo.User{ID: "x", Role: "guest"}, t1, lingo.ErrForbidden},
		{"reviewer other project", demoRev, t3, lingo.ErrNotFound},
		{"admin other project", otherAdmin, t1, lingo.ErrNotFound},
		{"missing", rita, 999999, lingo.ErrNotFound},
		{"no issue", rita, pending, lingo.ErrConflict},
		{"issue gone", rita, gone, lingo.ErrConflict},
	} {
		if r, err := all.ResyncTicketComments(ctx, c.u, c.id, lingo.CommentResyncOptions{}); !errors.Is(err, c.want) || r.TicketID != 0 {
			t.Errorf("%s: %+v %v, want %v", c.name, r, err, c.want)
		}
	}
	if r, err := all.ResyncTicketComments(ctx, otherAdmin, t3, lingo.CommentResyncOptions{}); err != nil || r.Added != 1 || r.Published != 1 {
		t.Errorf("admin own project: %+v %v", r, err)
	}
	// 7. Notify：新变为对外的评论照常发 ticket.comment 事件
	t4 := seedIssueTicket(t, db, "demo", "web", "u1", "open", 14)
	g.set("demo/web#14", gcomment(401, "dev", "内部：排期到下周", at))
	if _, err := marked.ResyncTicketComments(ctx, rita, t4, lingo.CommentResyncOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err = all.ResyncTicketComments(ctx, rita, t4, lingo.CommentResyncOptions{Notify: true})
	if err != nil || r.Published != 1 || !r.Notified {
		t.Fatalf("notify = %+v %v", r, err)
	}
	evs, _ := all.ListEvents(ctx, alice, 0, 200)
	found := false
	for _, e := range evs {
		if e.TicketID == t4 && e.Kind == lingo.EventTicketComment && e.Body == "内部：排期到下周" {
			found = true
		}
		if e.TicketID == t1 && e.Kind == lingo.EventTicketComment && strings.Contains(e.Body, "缓存") {
			t.Errorf("silent backfill visible in feed: %+v", e)
		}
	}
	if !found {
		t.Errorf("notify event missing: %+v", evs)
	}
	// 8. 回补后正常同步不受影响：Webhook 式的新评论照常保存并通知
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- all.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	issue := `"issue":{"id":112,"number":12,"user":{"login":"lingo-bot"},"body":"x","updated_at":"2026-11-18T01:00:00Z"},"repository":{"full_name":"demo/api"}`
	for i, c := range []struct {
		id     int
		body   string
		author string
	}{{105, "新的研发回复", "dev"}, {106, "机器人自己的评论", "lingo-bot"}} {
		body := fmt.Sprintf(`{"action":"created",%s,"comment":{"id":%d,"body":%q,"user":{"login":%q},"updated_at":%q},"sender":{"login":%q}}`,
			issue, c.id, c.body, c.author, time.Now().UTC().Format(time.RFC3339), c.author)
		if w := deliver(all.WebhookHandler(), fmt.Sprintf("resync-d%d", i), "issue_comment", body, "s"); w.Code != 200 {
			t.Fatalf("deliver: %d %s", w.Code, w.Body)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		cs, _ := all.ListTicketComments(ctx, alice, t1)
		if b := commentBodies(cs); len(b) == 2 && b[1] == "新的研发回复" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhook comment not synced after backfill: %v", commentBodies(cs))
		}
		time.Sleep(100 * time.Millisecond)
	}
	evs, _ = all.ListEvents(ctx, alice, 0, 200)
	if e := evs[len(evs)-1]; e.Kind != lingo.EventTicketComment || e.TicketID != t1 || e.Body != "新的研发回复" {
		t.Errorf("webhook comment event = %+v", e)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_ticket_comments WHERE gitea_comment_id = 106"); n != 0 {
		t.Errorf("bot webhook comment imported")
	}

	// REST
	rc := restClient{t: t, h: all.Handler()}
	code, obj, _ := rc.do("r1", "reviewer", "POST", fmt.Sprintf("/tickets/%d/comments/resync", t1), nil)
	if code != 200 || obj["ticket_id"] != float64(t1) || obj["issue"] != float64(12) || obj["fetched"] != float64(3) || obj["notified"] != false {
		t.Errorf("rest resync: %d %v", code, obj)
	}
	if code, _, _ := rc.do("r1", "reviewer", "POST", fmt.Sprintf("/tickets/%d/comments/resync", t1), map[string]any{"notify": true}); code != 200 {
		t.Errorf("rest notify body: %d", code)
	}
	for _, c := range []struct {
		user, role string
		id         int64
		code       int
	}{{"u1", "external", t1, 403}, {"a1", "agent", t1, 403}, {"r1", "reviewer", 999999, 404}, {"r1", "reviewer", pending, 409}, {"", "", t1, 401}} {
		if code, obj, _ := rc.do(c.user, c.role, "POST", fmt.Sprintf("/tickets/%d/comments/resync", c.id), nil); code != c.code {
			t.Errorf("rest %s %d: %d %v", c.role, c.id, code, obj)
		}
	}
}
