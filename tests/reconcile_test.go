package tests

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// TestReconcileRecoversMissedWebhooks 模拟 Webhook 全部丢失：对账按游标拉取 PR、Issue、评论与版本补齐。
func TestReconcileRecoversMissedWebhooks(t *testing.T) {
	db := testDB(t, true)
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		q := r.URL.Query()
		switch {
		case p == "/user":
			_, _ = w.Write([]byte(`{"id":1,"login":"lingo-bot"}`))
		case p == "/repos/demo/api/issues" && q.Get("type") == "pulls":
			_, _ = w.Write([]byte(`[{"id":300,"number":3,"title":"修复导出","body":"Fixes #12","state":"closed","pull_request":{"merged":true}}]`))
		case p == "/repos/demo/api/issues":
			_, _ = w.Write([]byte(`[{"id":100,"number":12,"title":"导出报错","state":"closed","updated_at":"2026-11-20T03:00:00Z"},
				{"id":101,"number":99,"title":"无关 Issue","state":"open","updated_at":"2026-11-20T03:00:00Z"}]`))
		case p == "/repos/demo/api/pulls/3":
			_, _ = w.Write([]byte(`{"id":300,"number":3,"title":"修复导出","body":"Fixes #12","merged":true,"merge_commit_sha":"abc123"}`))
		case p == "/repos/demo/api/issues/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 7, "body": "/reply 已修复", "user": map[string]any{"login": "dev"}, "issue_url": "http://g/demo/api/issues/12", "updated_at": "2026-11-20T02:00:00Z"},
				{"id": 8, "body": "内部", "user": map[string]any{"login": "dev"}, "issue_url": "http://g/demo/api/issues/12", "updated_at": "2026-11-20T02:01:00Z"},
				{"id": 9, "body": "补充", "user": map[string]any{"login": "lingo-bot"}, "issue_url": "http://g/demo/api/issues/12", "updated_at": "2026-11-20T02:02:00Z"},
			})
		case p == "/repos/demo/api/releases":
			now := time.Now().UTC().Format(time.RFC3339)
			_, _ = w.Write([]byte(`[{"id":50,"tag_name":"v1.2.0","name":"1.2","body":"","draft":false,"prerelease":false,"published_at":"` + now + `","html_url":"http://g/r/v1.2.0"}]`))
		case p == "/repos/demo/api/tags/v1.2.0":
			_, _ = w.Write([]byte(`{"name":"v1.2.0","commit":{"sha":"tag120"}}`))
		case p == "/repos/demo/api/compare/v1.2.0...abc123":
			_, _ = w.Write([]byte(`{"total_commits":0,"commits":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.Close)
	rec := &recorder{}
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{}, Notifier: rec, SkipSelfCheck: true, ReconcileInterval: time.Second, // 每秒对账一次：重复对账必须幂等
		Gitea:        lingo.GiteaConfig{BaseURL: g.URL, Token: "t", WebhookSecret: "s"},
		Projects:     []lingo.Project{{Key: "demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate: func(*http.Request) (lingo.User, error) { return lingo.User{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	id := seedTicketSynced(t, db)
	runLingo(t, l)
	evs := rec.waitFor(t, 3)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, string(e.Kind)+":"+e.Status)
	}
	// PR 合并 → fixed；Issue 已关闭（修复已记录，保持 fixed）；对外评论 → ticket.comment；版本 → released
	if got := strings.Join(kinds, ","); got != "ticket.status:fixed,ticket.comment:fixed,ticket.released:released" {
		t.Errorf("events = %s", got)
	}
	waitFor(t, func() bool {
		var n int
		_ = db.QueryRow("SELECT count(*) FROM lingo_repo_state WHERE kind <> 'ping'").Scan(&n)
		return n == 4
	})
	time.Sleep(2500 * time.Millisecond) // 再对账几轮
	if got := rec.waitFor(t, 3); len(got) != 3 {
		t.Errorf("repeated reconcile produced extra events: %d", len(got))
	}
	rows, _ := db.Query("SELECT kind FROM lingo_ticket_events WHERE ticket_id = $1 ORDER BY id", id)
	var hk []string
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		hk = append(hk, k)
	}
	rows.Close()
	if strings.Join(hk, ",") != "pull.merged,issue.synced,comment.edited,comment.edited,ticket.released" {
		t.Errorf("ticket_events = %v (want no duplicates from repeated runs)", hk)
	}
	var comments int
	if err := db.QueryRow("SELECT count(*) FROM lingo_ticket_comments WHERE ticket_id = $1", id).Scan(&comments); err != nil || comments != 2 {
		t.Errorf("comments = %d %v (bot comment skipped)", comments, err)
	}
}

// seedTicketSynced 建一张已转出、开发中的工单（Issue #12），项目与仓库已由 lingo 同步。
func seedTicketSynced(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`
		WITH p AS (INSERT INTO lingo_projects (key, name) VALUES ('demo', 'Demo') ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name RETURNING id),
		     r AS (INSERT INTO lingo_repos (project_id, owner, name) SELECT id, 'demo', 'api' FROM p
		           ON CONFLICT (lower(owner), lower(name)) DO UPDATE SET owner = EXCLUDED.owner RETURNING id, project_id)
		INSERT INTO lingo_tickets (user_id, project_id, repo_id, type, title, status, gitea_issue_number, gitea_updated_at)
		SELECT 'u1', project_id, id, 'bug', '导出报错', 'in_progress', 12, '2026-11-20T01:00:00Z' FROM r RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
