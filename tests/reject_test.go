package tests

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

var adam = lingo.User{ID: "a1", Name: "Adam", Role: lingo.RoleAdmin}

func waitIssueClose(t *testing.T, l *lingo.Lingo, id int64, state string) lingo.IssueClose {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := l.TicketIssueClose(context.Background(), rita, id)
		if err == nil && c.State == state {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("issue close of ticket %d: %+v %v, want %s", id, c, err, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (g *giteaServer) snapshot() (closes, comments []string, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.issues) > 0 {
		state, _ = g.issues[0]["state"].(string)
	}
	return append([]string(nil), g.closes...), append([]string(nil), g.comments...), state
}

func newForwarded(t *testing.T, g *giteaServer) (*lingo.Lingo, *sql.DB, func(string) int, int64) {
	t.Helper()
	l, db, _ := newTicketLingo(t, g, nil, map[lingo.TicketType]bool{lingo.TicketBug: true})
	runLingo(t, l)
	tk, err := l.CreateTicket(context.Background(), alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "登录失败",
		Fields: map[string][]string{"what": {"登录失败"}}})
	if err != nil {
		t.Fatal(err)
	}
	waitTicket(t, l, rita, tk.ID, "open")
	count := func(q string) (n int) {
		t.Helper()
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return l, db, count, tk.ID
}

// 驳回已转研发工单：保留工单、评论与历史，只关闭关联的主 Issue；并发 / 重复驳回只生效一次；
// 之后 Gitea 回流（灵问自己的关闭、研发重开、删除）不覆盖本地驳回结论；后台人员仍可随时评论（只留在灵问）。
func TestRejectForwardedClosesLinkedIssue(t *testing.T) {
	g := &giteaServer{templates: bugTemplate}
	l, db, count, id := newForwarded(t, g)
	ctx := context.Background()
	if _, err := l.AddTicketComment(ctx, rita, id, "我们先看一下", nil); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "staff comment synced", func() bool { _, cs, _ := g.snapshot(); return len(cs) == 1 })

	// 并发驳回（如接入方重试）：只有一次生效
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tk, err := l.RejectForwardedTicket(ctx, rita, id, "不在支持范围")
			if err == nil && tk.Status != "rejected" {
				err = errors.New("status " + tk.Status)
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("reject: %v", err)
		}
	}
	tk, err := l.GetTicket(ctx, alice, id)
	if err != nil || tk.Status != "rejected" || !strings.HasSuffix(tk.IssueURL, "/demo/api/issues/40") {
		t.Fatalf("ticket = %+v %v", tk, err)
	}
	c := waitIssueClose(t, l, id, "done")
	if c.Repo != "demo/api" || c.Number != 40 || c.Attempt != 1 || c.Failures != 0 {
		t.Errorf("close = %+v", c)
	}
	closes, _, state := g.snapshot()
	if len(closes) != 1 || closes[0] != "/repos/demo/api/issues/40" || state != "closed" {
		t.Errorf("gitea closes = %v state %s", closes, state)
	}
	if n := count("SELECT count(*) FROM lingo_outbox WHERE payload->>'op' = 'close_issue'"); n != 1 {
		t.Errorf("close tasks = %d", n)
	}
	if n := count("SELECT count(*) FROM lingo_events WHERE kind = 'ticket.status' AND status = 'rejected'"); n != 1 {
		t.Errorf("rejected events = %d", n)
	}
	if n := count("SELECT count(*) FROM lingo_ticket_events WHERE kind = 'ticket.rejected'"); n != 1 {
		t.Errorf("rejected history = %d", n)
	}
	evs, err := l.ListEvents(ctx, alice, 0, 0)
	if err != nil || evs[len(evs)-1].Status != "rejected" || evs[len(evs)-1].Body != "不在支持范围" {
		t.Errorf("submitter events = %+v %v", evs, err)
	}

	// 驳回后后台人员仍可评论：保存并对提交人可见，但不再发到已关闭的 Issue
	if _, err := l.AddTicketComment(ctx, adam, id, "如有新情况请重新提交", nil); err != nil {
		t.Fatalf("staff comment after reject: %v", err)
	}
	cs, err := l.ListTicketComments(ctx, alice, id)
	if err != nil || len(cs) != 2 || cs[0].Body != "我们先看一下" || cs[1].Body != "如有新情况请重新提交" {
		t.Errorf("comments = %+v %v", cs, err)
	}

	// Gitea 回流：灵问关闭 Issue 的回声、研发重开、Issue 被删除，工单都保持 rejected
	h := l.WebhookHandler()
	issue := `"issue":{"id":1000,"number":40,"user":{"login":"lingo-bot"},"body":"x","updated_at":"2026-11-21T01:00:00Z"},"repository":{"full_name":"demo/api"}`
	if w := deliver(h, "r1", "issues", `{"action":"closed",`+issue+`,"sender":{"login":"lingo-bot"}}`, "s"); w.Code != 200 {
		t.Fatalf("deliver: %d", w.Code)
	}
	g.mu.Lock()
	g.issues[0]["state"] = "open"
	g.mu.Unlock()
	if w := deliver(h, "r2", "issues", `{"action":"reopened",`+issue+`,"sender":{"login":"dev-agent"}}`, "s"); w.Code != 200 {
		t.Fatalf("deliver: %d", w.Code)
	}
	if w := deliver(h, "r3", "issues", `{"action":"deleted",`+issue+`,"sender":{"login":"dev-agent"}}`, "s"); w.Code != 200 {
		t.Fatalf("deliver: %d", w.Code)
	}
	waitProcessed(t, db, 3)
	if tk, err := l.GetTicket(ctx, rita, id); err != nil || tk.Status != "rejected" {
		t.Errorf("after gitea events: %+v %v", tk, err)
	}
	if n := count("SELECT count(*) FROM lingo_events WHERE kind = 'ticket.status' AND id > (SELECT max(id) FROM lingo_events WHERE status = 'rejected' AND kind = 'ticket.status')"); n != 0 {
		t.Errorf("status events after rejection = %d", n)
	}
	if n := count("SELECT count(*) FROM lingo_ticket_events WHERE source = 'gitea'"); n != 3 {
		t.Errorf("gitea history = %d, want 3 (recorded, not applied)", n)
	}
	if closes, _, _ := g.snapshot(); len(closes) != 1 {
		t.Errorf("reopened issue was closed again: %v", closes)
	}
}

// 关闭失败可查、可重试：不可重试的错误直接放弃（failed，带原因），修复后重试入队新一轮并完成；重复重试不重复入队。
func TestRejectForwardedCloseFailureAndRetry(t *testing.T) {
	g := &giteaServer{templates: bugTemplate}
	l, _, count, id := newForwarded(t, g)
	ctx := context.Background()
	g.mu.Lock()
	g.closeErr = 403
	g.mu.Unlock()
	if _, err := l.RejectForwardedTicket(ctx, adam, id, "重复需求"); err != nil {
		t.Fatal(err)
	}
	c := waitIssueClose(t, l, id, "failed")
	if c.Attempt != 1 || !strings.Contains(c.LastError, "403") || c.UpdatedAt == nil {
		t.Errorf("failed close = %+v", c)
	}
	if tk, _ := l.GetTicket(ctx, rita, id); tk.Status != "rejected" {
		t.Errorf("status after close failure = %s", tk.Status)
	}
	if _, err := l.TicketIssueClose(ctx, alice, id); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("external reads close state: %v", err)
	}
	if _, err := l.RetryTicketIssueClose(ctx, alice, id); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("external retries: %v", err)
	}
	g.mu.Lock()
	g.closeErr = 0
	g.mu.Unlock()
	c, err := l.RetryTicketIssueClose(ctx, rita, id)
	if err != nil || c.Attempt != 2 || (c.State != "pending" && c.State != "done") {
		t.Fatalf("retry = %+v %v", c, err)
	}
	c = waitIssueClose(t, l, id, "done")
	if c.Attempt != 2 || c.LastError != "" {
		t.Errorf("done = %+v", c)
	}
	if c, err := l.RetryTicketIssueClose(ctx, rita, id); err != nil || c.State != "done" || c.Attempt != 2 {
		t.Errorf("retry after done = %+v %v", c, err)
	}
	if closes, _, state := g.snapshot(); len(closes) != 1 || state != "closed" {
		t.Errorf("closes = %v state %s", closes, state)
	}
	if n := count("SELECT count(*) FROM lingo_outbox WHERE payload->>'op' = 'close_issue'"); n != 2 {
		t.Errorf("close tasks = %d, want 2 rounds", n)
	}
	if n := count("SELECT count(*) FROM lingo_ticket_events WHERE kind = 'issue.close_retry'"); n != 1 {
		t.Errorf("retry history = %d", n)
	}
}

func TestRESTRejectForwarded(t *testing.T) {
	l, c := newRESTLingo(t)
	runLingo(t, l)
	code, created, _ := c.do("u1", "external", "POST", "/tickets", map[string]any{"type": "bug", "content": "导出报错"})
	if code != 201 || created["status"] != "submitted" {
		t.Fatalf("create: %d %v", code, created)
	}
	id := int64(created["id"].(float64))
	path := "/tickets/" + jsonNum(id)
	waitUntil(t, "issue created", func() bool {
		_, tk, _ := c.do("r1", "reviewer", "GET", path, nil)
		return tk["status"] == "open"
	})
	for _, x := range []struct {
		user, role string
		body       any
		code       int
		errCode    string
	}{
		{"u1", "external", map[string]any{"reason": "x"}, 403, "forbidden"},
		{"g1", "agent", map[string]any{"reason": "x"}, 403, "forbidden"},
		{"r1", "reviewer", map[string]any{}, 400, "invalid_input"},
	} {
		code, body, _ := c.do(x.user, x.role, "POST", path+"/reject-forwarded", x.body)
		if code != x.code || body["error"].(map[string]any)["code"] != x.errCode {
			t.Errorf("%s: %d %v", x.role, code, body)
		}
	}
	code, tk, _ := c.do("r1", "reviewer", "POST", path+"/reject-forwarded", map[string]any{"reason": "不修复"})
	if code != 200 || tk["status"] != "rejected" || tk["issue_url"] == nil {
		t.Fatalf("reject: %d %v", code, tk)
	}
	waitUntil(t, "issue closed", func() bool {
		_, st, _ := c.do("r1", "reviewer", "GET", path+"/issue-close", nil)
		return st["state"] == "done"
	})
	code, st, _ := c.do("a1", "admin", "POST", path+"/issue-close/retry", nil)
	if code != 200 || st["state"] != "done" || st["issue"] != float64(40) || st["repo"] != "demo/api" || st["attempt"] != float64(1) {
		t.Errorf("retry after done: %d %v", code, st)
	}
	if code, body, _ := c.do("u1", "external", "GET", path+"/issue-close", nil); code != 403 {
		t.Errorf("external close state: %d %v", code, body)
	}
	// 待确认工单走原有 reject；对它调 reject-forwarded 返回 409
	_, pending, _ := c.do("u1", "external", "POST", "/tickets", map[string]any{"type": "feature", "content": "希望支持导出"})
	ppath := "/tickets/" + jsonNum(int64(pending["id"].(float64)))
	if code, body, _ := c.do("r1", "reviewer", "POST", ppath+"/reject-forwarded", map[string]any{"reason": "x"}); code != 409 {
		t.Errorf("pending_triage: %d %v", code, body)
	}
	if code, body, _ := c.do("r1", "reviewer", "POST", ppath+"/issue-close/retry", nil); code != 409 {
		t.Errorf("retry without issue: %d %v", code, body)
	}
}

// 建单与驳回并发：Issue 在 Gitea 建好、结果尚未记下时工单被驳回。补记关联并关闭该 Issue，
// 工单保持 rejected，不再补传附件、不改 Issue 正文。
func TestRejectDuringIssueCreation(t *testing.T) {
	g := &giteaServer{templates: bugTemplate}
	l, _, _ := newTicketLingo(t, g, nil, map[lingo.TicketType]bool{lingo.TicketBug: true})
	marker := regexp.MustCompile(`lingo:ticket=(\d+)`)
	var rejectErr error
	g.onCreate = func(body string) {
		m := marker.FindStringSubmatch(body)
		if m == nil {
			rejectErr = errors.New("no ticket marker in issue body")
			return
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		_, rejectErr = l.RejectForwardedTicket(context.Background(), rita, id, "重复需求")
	}
	runLingo(t, l)
	ctx := context.Background()
	tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "登录失败",
		Fields:      map[string][]string{"what": {"登录失败"}},
		Attachments: []lingo.Attachment{{ID: "a", Name: "a.png", URL: "https://app.example.com/files/a", ContentType: "image/png"}}})
	if err != nil {
		t.Fatal(err)
	}
	c := waitIssueClose(t, l, tk.ID, "done")
	g.mu.Lock()
	defer g.mu.Unlock()
	if rejectErr != nil {
		t.Fatalf("reject during create: %v", rejectErr)
	}
	if c.Repo != "demo/api" || c.Number != 40 || len(g.created) != 1 || len(g.closes) != 1 || g.issues[0]["state"] != "closed" {
		t.Errorf("close = %+v created %d closes %v", c, len(g.created), g.closes)
	}
	if len(g.edits) != 0 || len(g.assets) != 0 {
		t.Errorf("attachments forwarded after reject: edits %q assets %v", g.edits, g.assets)
	}
	got, err := l.GetTicket(ctx, rita, tk.ID)
	if err != nil || got.Status != "rejected" || !strings.HasSuffix(got.IssueURL, "/demo/api/issues/40") {
		t.Errorf("ticket = %+v %v", got, err)
	}
}

// 关闭时 Issue 返回 404 而仓库也读不到（机器人权限被收回）：无法确认 Issue 已删除，任务 failed、原因可查，
// 工单保持 rejected；权限恢复后人工重试关闭成功。
func TestRejectForwardedCloseNotFoundRepoHidden(t *testing.T) {
	g := &giteaServer{templates: bugTemplate}
	l, _, count, id := newForwarded(t, g)
	ctx := context.Background()
	g.mu.Lock()
	g.hidden = true
	g.mu.Unlock()
	if _, err := l.RejectForwardedTicket(ctx, rita, id, "不在支持范围"); err != nil {
		t.Fatal(err)
	}
	c := waitIssueClose(t, l, id, "failed")
	if c.Attempt != 1 || !strings.Contains(c.LastError, "repository not accessible") {
		t.Errorf("close = %+v", c)
	}
	if tk, err := l.GetTicket(ctx, rita, id); err != nil || tk.Status != "rejected" {
		t.Errorf("ticket = %+v %v", tk, err)
	}
	g.mu.Lock()
	g.hidden = false
	g.mu.Unlock()
	if r, err := l.RetryTicketIssueClose(ctx, rita, id); err != nil || r.State != "pending" || r.Attempt != 2 {
		t.Fatalf("retry = %+v %v", r, err)
	}
	waitIssueClose(t, l, id, "done")
	closes, _, state := g.snapshot()
	if len(closes) != 1 || state != "closed" {
		t.Errorf("gitea closes = %v state %s", closes, state)
	}
	if n := count("SELECT count(*) FROM lingo_outbox WHERE payload->>'op' = 'close_issue'"); n != 2 {
		t.Errorf("close tasks = %d", n)
	}
}

// 关闭时 Issue 返回 404 而仓库可读：确认 Issue 已删除，按幂等完成，不调用关闭。
func TestRejectForwardedCloseIssueDeleted(t *testing.T) {
	g := &giteaServer{templates: bugTemplate}
	l, _, _, id := newForwarded(t, g)
	g.mu.Lock()
	g.issues = nil
	g.mu.Unlock()
	if _, err := l.RejectForwardedTicket(context.Background(), rita, id, "不在支持范围"); err != nil {
		t.Fatal(err)
	}
	if c := waitIssueClose(t, l, id, "done"); c.LastError != "" {
		t.Errorf("close = %+v", c)
	}
	if closes, _, _ := g.snapshot(); len(closes) != 0 {
		t.Errorf("gitea closes = %v", closes)
	}
}
