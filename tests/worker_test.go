package tests

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// seedTicket 建项目 demo、仓库 demo/api 与一张已转出的工单（Issue #12），返回工单 ID。
func seedTicket(t *testing.T, db *sql.DB, status string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`
		WITH p AS (INSERT INTO lingo_projects (key, name) VALUES ('demo', 'Demo') RETURNING id),
		     r AS (INSERT INTO lingo_repos (project_id, owner, name, kind) SELECT id, 'demo', 'api', 'backend' FROM p RETURNING id, project_id)
		INSERT INTO lingo_tickets (user_id, project_id, repo_id, type, title, status, gitea_issue_number)
		SELECT 'u1', project_id, id, 'bug', '登录失败', $1, 12 FROM r RETURNING id`, status).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type recorder struct {
	mu     sync.Mutex
	events []lingo.Event
}

func (r *recorder) Notify(_ context.Context, e lingo.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) waitFor(t *testing.T, n int) []lingo.Event {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r.mu.Lock()
		got := append([]lingo.Event(nil), r.events...)
		r.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			if len(got) < n {
				t.Fatalf("got %d events, want %d: %+v", len(got), n, got)
			}
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// fakeGitea 模拟 GET /user 与 GET /repos/demo/api/issues/12，Issue 状态可在测试中修改。
type fakeGitea struct {
	mu    sync.Mutex
	issue map[string]any
}

func (f *fakeGitea) set(kv map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range kv {
		f.issue[k] = v
	}
}

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/v1/user":
		_, _ = w.Write([]byte(`{"id":1,"login":"lingo-bot"}`))
	case "/api/v1/repos/demo/api/issues/12":
		_ = json.NewEncoder(w).Encode(f.issue)
	default:
		http.NotFound(w, r)
	}
}

func TestWorkerSyncsTicketFromWebhooks(t *testing.T) {
	db := testDB(t, true)
	id := seedTicket(t, db, "open")
	fg := &fakeGitea{issue: map[string]any{"id": 100, "number": 12, "state": "open", "updated_at": "2026-11-18T01:00:00Z",
		"assignees": []map[string]any{{"login": "dev-agent"}}}}
	gsrv := httptest.NewServer(fg)
	t.Cleanup(gsrv.Close)
	rec := &recorder{}
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{}, Notifier: rec,
		Gitea:        lingo.GiteaConfig{BaseURL: gsrv.URL, Token: "t", WebhookSecret: "s"},
		Projects:     []lingo.Project{{Key: "demo", Name: "Demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate: func(*http.Request) (lingo.User, error) { return lingo.User{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	h := l.WebhookHandler()
	send := func(id, event, body string) {
		t.Helper()
		if w := deliver(h, id, event, body, "s"); w.Code != 200 {
			t.Fatalf("deliver %s: %d %s", id, w.Code, w.Body)
		}
	}
	issue := `"issue":{"id":100,"number":12,"user":{"login":"lingo-bot"},"body":"x","updated_at":"2026-11-18T01:00:00Z"},"repository":{"full_name":"demo/api"}`

	// 1. 领取：open → in_progress
	send("d1", "issue_assign", `{"action":"assigned",`+issue+`,"sender":{"login":"dev-agent"}}`)
	evs := rec.waitFor(t, 1)
	if e := evs[0]; e.Kind != lingo.EventTicketStatus || e.PrevStatus != "open" || e.Status != "in_progress" ||
		e.TicketID != id || e.ProjectKey != "demo" || e.UserID != "u1" || e.Title != "登录失败" || e.TicketType != lingo.TicketBug ||
		e.URL != gsrv.URL+"/demo/api/issues/12" || e.ID == 0 {
		t.Errorf("assigned event = %+v", e)
	}

	// 2. 内部评论不通知；/reply 评论通知提交人；机器人自己的评论忽略
	comment := func(cid int, body, author string) string {
		return fmt.Sprintf(`{"action":"created",%s,"comment":{"id":%d,"body":%q,"user":{"login":%q},"updated_at":"2026-11-18T02:00:00Z"},"sender":{"login":%q}}`,
			issue, cid, body, author, author)
	}
	send("d2", "issue_comment", comment(1, "内部讨论", "dev-agent"))
	send("d3", "issue_comment", comment(2, "/reply 已修复，请重试", "dev-agent"))
	send("d4", "issue_comment", comment(3, "来自提交人的补充", "lingo-bot"))
	evs = rec.waitFor(t, 2)
	if e := evs[1]; e.Kind != lingo.EventTicketComment || e.Body != "已修复，请重试" {
		t.Errorf("comment event = %+v", e)
	}

	// 3. 修复 PR 合并后 Issue 被自动关闭：in_progress → fixed，关闭不再产生事件
	send("d5", "pull_request", `{"action":"closed","number":3,"pull_request":{"id":300,"number":3,"title":"修复登录","body":"Fixes #12",
		"merged":true,"merge_commit_sha":"abc123","updated_at":"2026-11-18T03:00:00Z"},"repository":{"full_name":"demo/api"},"sender":{"login":"dev-agent"}}`)
	evs = rec.waitFor(t, 3)
	if e := evs[2]; e.Kind != lingo.EventTicketStatus || e.PrevStatus != "in_progress" || e.Status != "fixed" {
		t.Errorf("merged event = %+v", e)
	}
	fg.set(map[string]any{"state": "closed", "updated_at": "2026-11-18T03:00:01Z"})
	send("d6", "issues", `{"action":"closed",`+issue+`,"sender":{"login":"dev-agent"}}`)
	waitProcessed(t, db, 6)

	var status, commit string
	var pr int64
	if err := db.QueryRow("SELECT status, fixed_pr, fixed_commit FROM lingo_tickets WHERE id = $1", id).Scan(&status, &pr, &commit); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" || pr != 3 || commit != "abc123" {
		t.Errorf("ticket = %s %d %s", status, pr, commit)
	}
	if got := rec.waitFor(t, 3); len(got) != 3 {
		t.Errorf("unexpected extra events: %+v", got[3:])
	}
	count := func(q string) (n int) {
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("SELECT count(*) FROM lingo_ticket_comments WHERE source = 'gitea'"); n != 2 {
		t.Errorf("gitea comments = %d, want 2 (bot comment skipped)", n)
	}
	if n := count("SELECT count(*) FROM lingo_ticket_comments WHERE public"); n != 1 {
		t.Errorf("public comments = %d", n)
	}
	// 审计：assigned、2 条评论、merged、closed
	if n := count("SELECT count(*) FROM lingo_ticket_events WHERE source = 'gitea'"); n != 5 {
		t.Errorf("ticket_events = %d", n)
	}
	if n := count("SELECT count(*) FROM lingo_outbox WHERE kind = 'notify' AND status = 'done'"); n != 3 {
		t.Errorf("delivered outbox = %d", n)
	}
}

// waitProcessed 等待前 n 条投递全部处理完成。
func waitProcessed(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var done int
		var errs string
		if err := db.QueryRow("SELECT count(*) FROM lingo_webhook_deliveries WHERE processed_at IS NOT NULL").Scan(&done); err != nil {
			t.Fatal(err)
		}
		if done >= n {
			return
		}
		if time.Now().After(deadline) {
			_ = db.QueryRow("SELECT COALESCE(string_agg(delivery_id || ': ' || last_error, '; '), '') FROM lingo_webhook_deliveries WHERE processed_at IS NULL").Scan(&errs)
			t.Fatalf("processed %d/%d deliveries; pending: %s", done, n, strings.TrimSpace(errs))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
