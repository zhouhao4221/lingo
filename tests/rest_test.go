package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// restClient 以请求头 X-User / X-Role 声明身份调用 Handler。
type restClient struct {
	t *testing.T
	h http.Handler
}

func (c restClient) do(user, role, method, path string, body any) (int, map[string]any, []any) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	if method != "GET" {
		r.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		r.Header.Set("X-User", user)
		r.Header.Set("X-Role", role)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	var obj map[string]any
	var arr []any
	if strings.HasPrefix(strings.TrimSpace(w.Body.String()), "[") {
		_ = json.Unmarshal(w.Body.Bytes(), &arr)
	} else if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &obj)
	}
	return w.Code, obj, arr
}

func newRESTLingo(t *testing.T) (*lingo.Lingo, restClient) {
	t.Helper()
	db := testDB(t, true)
	g := &giteaServer{templates: bugTemplate}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{},
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		Projects: []lingo.Project{{Key: "demo", Name: "Demo", AutoForward: map[lingo.TicketType]bool{lingo.TicketBug: true},
			Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate: func(r *http.Request) (lingo.User, error) {
			if r.Header.Get("X-User") == "" {
				return lingo.User{}, errors.New("anonymous")
			}
			return lingo.User{ID: r.Header.Get("X-User"), Name: r.Header.Get("X-User"), Role: lingo.Role(r.Header.Get("X-Role"))}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l, restClient{t: t, h: l.Handler()}
}

func TestRESTTicketFlow(t *testing.T) {
	_, c := newRESTLingo(t)
	const ext, rev = "external", "reviewer"

	if code, _, _ := c.do("", "", "GET", "/tickets/mine", nil); code != 401 {
		t.Errorf("anonymous: %d", code)
	}
	code, _, types := c.do("u1", ext, "GET", "/ticket-types", nil)
	if code != 200 || len(types) != 3 {
		t.Fatalf("ticket-types: %d %v", code, types)
	}
	bug := types[1].(map[string]any)
	if bug["type"] != "bug" || bug["template"] != "Bug" || len(bug["fields"].([]any)) != 2 {
		t.Errorf("bug type = %v", bug)
	}

	code, created, _ := c.do("u1", ext, "POST", "/tickets", map[string]any{"type": "feature", "content": "希望支持导出", "context": map[string]string{"page": "/r"}})
	if code != 201 || created["status"] != "pending_triage" || created["project"] != "demo" {
		t.Fatalf("create: %d %v", code, created)
	}
	id := int64(created["id"].(float64))
	path := "/tickets/" + jsonNum(id)
	if code, body, _ := c.do("u1", ext, "POST", "/tickets", map[string]any{"type": "question", "content": "x"}); code != 400 ||
		body["error"].(map[string]any)["code"] != "invalid_input" {
		t.Errorf("bad type: %d %v", code, body)
	}
	if code, _, mine := c.do("u1", ext, "GET", "/tickets/mine", nil); code != 200 || len(mine) != 1 {
		t.Errorf("mine: %d %v", code, mine)
	}
	if code, _, mine := c.do("u2", ext, "GET", "/tickets/mine", nil); code != 200 || len(mine) != 0 {
		t.Errorf("other's mine: %d %v", code, mine)
	}
	if code, _, _ := c.do("u2", ext, "GET", path, nil); code != 404 {
		t.Errorf("other user detail: %d", code)
	}

	// 后台：列表、统计、驳回校验、转出
	if code, _, _ := c.do("u1", ext, "GET", "/triage", nil); code != 403 {
		t.Errorf("external triage: %d", code)
	}
	if code, _, list := c.do("r1", rev, "GET", "/triage?type=feature", nil); code != 200 || len(list) != 1 {
		t.Errorf("triage: %d %v", code, list)
	}
	if code, _, sum := c.do("r1", rev, "GET", "/triage/summary", nil); code != 200 || len(sum) != 1 || sum[0].(map[string]any)["pending"] != float64(1) {
		t.Errorf("summary: %d %v", code, sum)
	}
	if code, _, _ := c.do("r1", rev, "POST", path+"/reject", map[string]any{"reason": ""}); code != 400 {
		t.Errorf("reject without reason: %d", code)
	}
	if code, body, _ := c.do("r1", rev, "POST", path+"/answer", map[string]any{"answer": "已支持，在设置 > 导出"}); code != 200 || body["status"] != "answered" {
		t.Fatalf("answer: %d %v", code, body)
	}
	if code, _, _ := c.do("r1", rev, "POST", path+"/forward", nil); code != 409 {
		t.Errorf("forward answered: %d", code)
	}
	code, detail, _ := c.do("u1", ext, "GET", path, nil)
	if code != 200 || detail["status"] != "answered" || len(detail["comments"].([]any)) != 1 {
		t.Errorf("detail: %d %v", code, detail)
	}

	// 变更流：提交人看不到 triage.*；后台看得到
	_, _, extEvents := c.do("u1", ext, "GET", "/events", nil)
	_, _, revEvents := c.do("r1", rev, "GET", "/events", nil)
	kinds := func(es []any) string {
		var out []string
		for _, e := range es {
			out = append(out, e.(map[string]any)["kind"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := kinds(extEvents); got != "ticket.created,ticket.status" {
		t.Errorf("external events = %s", got)
	}
	if got := kinds(revEvents); got != "ticket.created,triage.required,ticket.status,triage.resolved" {
		t.Errorf("reviewer events = %s", got)
	}
	last := int64(revEvents[1].(map[string]any)["id"].(float64))
	if _, _, rest := c.do("r1", rev, "GET", "/events?after="+jsonNum(last), nil); kinds(rest) != "ticket.status,triage.resolved" {
		t.Errorf("events after cursor = %s", kinds(rest))
	}

	// agent 只看已转出工单在转出之后的事件：这张被直接答复的工单对 agent 不可见
	if _, _, agentEvents := c.do("a1", "agent", "GET", "/events", nil); len(agentEvents) != 0 {
		t.Errorf("agent events = %s", kinds(agentEvents))
	}
	// 写接口必须是 JSON（CSRF 防护）
	r := httptest.NewRequest("POST", path+"/reject", strings.NewReader(`{"reason":"x"}`))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("X-User", "r1")
	r.Header.Set("X-Role", rev)
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain post: %d", w.Code)
	}

	// 开关
	if code, _, _ := c.do("r1", rev, "PUT", "/settings/auto-forward", map[string]any{"project": "demo", "type": "feature", "on": true}); code != 403 {
		t.Errorf("reviewer put setting: %d", code)
	}
	if code, _, _ := c.do("root", "admin", "PUT", "/settings/auto-forward", map[string]any{"project": "demo", "type": "feature", "on": true}); code != 204 {
		t.Errorf("admin put setting: %d", code)
	}
	_, _, settings := c.do("r1", rev, "GET", "/settings/auto-forward", nil)
	got := map[string]string{}
	for _, s := range settings {
		m := s.(map[string]any)
		got[m["type"].(string)] = jsonNum(map[bool]int64{true: 1}[m["on"].(bool)]) + "/" + jsonNum(map[bool]int64{true: 1}[m["overridden"].(bool)])
	}
	if got["bug"] != "1/0" || got["feature"] != "1/1" || got["consult"] != "0/0" {
		t.Errorf("settings = %v", got)
	}
	if code, _, _ := c.do("root", "admin", "PUT", "/settings/auto-forward", map[string]any{"project": "demo", "type": "feature", "on": nil}); code != 204 {
		t.Errorf("admin reset: %d", code)
	}
}

func TestEventsHideUncommitted(t *testing.T) {
	db := testDB(t, true)
	id := seedTicket(t, db, "open")
	s, _ := store.New(db)
	svc, _ := ticket.NewService(ticket.ServiceOptions{Store: s})
	ctx := context.Background()
	admin := lingoPrincipal()
	// 一个未提交的事务先拿到较小的事件 ID
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO events (kind, ticket_id) VALUES ('ticket.status', $1)", id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO events (kind, ticket_id) VALUES ('ticket.comment', $1)", id); err != nil {
		t.Fatal(err)
	}
	es, err := svc.ListEvents(ctx, admin, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 0 {
		t.Errorf("returned events past an uncommitted one: %+v", es)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if es, _ := svc.ListEvents(ctx, admin, 0, 0); len(es) != 2 {
		t.Errorf("after commit = %d", len(es))
	}
}

func lingoPrincipal() auth.Principal { return auth.Principal{ID: "root", Role: auth.RoleAdmin} }

func TestCommentEventRedactedWhenDeleted(t *testing.T) {
	db := testDB(t, true)
	seedTicket(t, db, "open")
	s, _ := store.New(db)
	sync, _ := ticket.NewSync(ticket.SyncOptions{Store: s})
	svc, _ := ticket.NewService(ticket.ServiceOptions{Store: s})
	ctx := context.Background()
	u := ticket.CommentUpdate{Change: ticket.CommentCreated, Repo: "demo/api", Number: 12, CommentID: 5, Body: "密钥是 abc", Public: true}
	if err := sync.ApplyComment(ctx, u); err != nil {
		t.Fatal(err)
	}
	ext := auth.Principal{ID: "u1", Role: auth.RoleExternal}
	if es, _ := svc.ListEvents(ctx, ext, 0, 0); len(es) != 1 || es[0].Body != "密钥是 abc" {
		t.Fatalf("events = %+v", es)
	}
	u.Change = ticket.CommentDeleted
	if err := sync.ApplyComment(ctx, u); err != nil {
		t.Fatal(err)
	}
	if es, _ := svc.ListEvents(ctx, ext, 0, 0); len(es) != 1 || es[0].Body != "" {
		t.Errorf("deleted comment still in feed: %+v", es)
	}
}

func TestRemindOverdueStore(t *testing.T) {
	db := testDB(t, true)
	id := seedTicket(t, db, "pending_triage")
	if _, err := db.Exec("UPDATE tickets SET triage_since = now() - interval '25 hours' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	s, _ := store.New(db, store.WithNotify())
	svc, _ := ticket.NewService(ticket.ServiceOptions{Store: s})
	ctx := context.Background()
	for i, want := range []int{1, 0} {
		n, err := svc.RemindOverdue(ctx, 24*time.Hour, 10)
		if err != nil || n != want {
			t.Errorf("round %d: %d %v", i, n, err)
		}
	}
	var events, outbox int
	if err := db.QueryRow("SELECT (SELECT count(*) FROM events WHERE kind = 'triage.overdue'), (SELECT count(*) FROM outbox WHERE kind = 'notify')").Scan(&events, &outbox); err != nil || events != 1 || outbox != 1 {
		t.Errorf("events %d outbox %d %v", events, outbox, err)
	}
}
