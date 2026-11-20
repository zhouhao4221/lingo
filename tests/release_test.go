package tests

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo"
)

// releaseGitea 模拟 /user、tag 与提交比较接口。
func releaseGitea(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch p {
		case "/user":
			_, _ = w.Write([]byte(`{"id":1,"login":"lingo-bot"}`))
		case "/repos/demo/api/tags/v1.1.0":
			_, _ = w.Write([]byte(`{"name":"v1.1.0","commit":{"sha":"tagsha110"}}`))
		case "/repos/demo/web/tags/v2.0.0":
			_, _ = w.Write([]byte(`{"name":"v2.0.0","commit":{"sha":"websha"}}`))
		case "/repos/demo/web/compare/v2.0.0...webfix":
			_, _ = w.Write([]byte(`{"total_commits":0,"commits":[]}`))
		default:
			// compare/<ref>...<sha>：sha 已包含在 ref 中时没有新提交
			if rest, ok := strings.CutPrefix(p, "/repos/demo/api/compare/"); ok {
				base, head, _ := strings.Cut(rest, "...")
				contained := (base == "v1.1.0" || base == "tagsha110") && strings.EqualFold(head, "abc123")
				n := 1
				if contained {
					n = 0
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_commits": n, "commits": []map[string]any{}})
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func seedRelease(t *testing.T, db *sql.DB) (byCommit, byRef, untouched int64) {
	t.Helper()
	ids := make([]int64, 3)
	for i, c := range []struct {
		issue  int
		commit string
	}{{12, "ABC123"}, {13, ""}, {14, "yyy"}} {
		if err := db.QueryRow(`INSERT INTO lingo_tickets (user_id, project_id, repo_id, type, title, status, gitea_issue_number, fixed_commit)
			SELECT 'u1', p.id, r.id, 'bug', 't', 'fixed', $1, $2 FROM lingo_projects p JOIN lingo_repos r ON r.project_id = p.id
			WHERE p.key = 'demo' AND r.name = 'api' RETURNING id`, c.issue, c.commit).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO lingo_releases (repo_id, tag, published_at, commit_sha)
		SELECT id, 'v1.0.0', now() - interval '7 days', 'sha100' FROM lingo_repos WHERE name = 'api'`); err != nil {
		t.Fatal(err)
	}
	return ids[0], ids[1], ids[2]
}

func TestReleaseAndDeployFlow(t *testing.T) {
	db := testDB(t, true)
	srv := releaseGitea(t)
	rec := &recorder{}
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{}, Notifier: rec,
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		Projects: []lingo.Project{{Key: "demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api",
			Deploy: lingo.DeploySignal{Workflow: "deploy-prod.yml", StatusContext: "deploy/production"}}, {Owner: "demo", Name: "web"}}}},
		Authenticate: func(*http.Request) (lingo.User, error) { return lingo.User{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 先同步项目，再造数据
	if err := l.SetAutoForward(ctx, lingo.User{ID: "root", Role: lingo.RoleAdmin}, "demo", lingo.TicketBug, false); err != nil {
		t.Fatal(err)
	}
	byCommit, byRef, untouched := seedRelease(t, db)
	if err := l.MarkDeployed(ctx, "demo/api", "v9.9.9", ""); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("unknown tag: %v", err)
	}
	runLingo(t, l)
	h := l.WebhookHandler()
	repo := `"repository":{"full_name":"demo/api"},"sender":{"login":"dev"}`

	// 预发布：只记录关联，不推进状态
	send := func(id, event, body string) {
		t.Helper()
		if w := deliver(h, id, event, body, "s"); w.Code != 200 {
			t.Fatalf("deliver %s: %d %s", id, w.Code, w.Body)
		}
	}
	send("r0", "release", `{"action":"published","release":{"id":9,"tag_name":"v1.1.0-rc.1","name":"rc","body":"#13","prerelease":true,
		"published_at":"2026-11-19T00:00:00Z","html_url":"http://g/demo/api/releases/v1.1.0-rc.1"},`+repo+`}`)
	// 正式版本：#12 的修复提交包含在 tag 中，#13 修复提交未知、按说明中的引用补漏，#14 的修复提交不在版本中
	send("r1", "release", `{"action":"published","release":{"id":10,"tag_name":"v1.1.0","name":"1.1","body":"修复 #13","prerelease":false,
		"published_at":"2026-11-20T00:00:00Z","html_url":"http://g/demo/api/releases/v1.1.0"},`+repo+`}`)
	evs := rec.waitFor(t, 2)
	got := map[int64]string{}
	for _, e := range evs {
		if e.Kind != lingo.EventTicketReleased || e.Status != "released" || e.URL != "http://g/demo/api/releases/v1.1.0" || e.Body != "已在 v1.1.0 发布" {
			t.Errorf("event = %+v", e)
		}
		got[e.TicketID] = e.Status
	}
	if got[byCommit] == "" || got[byRef] == "" || got[untouched] != "" {
		t.Errorf("released = %v", got)
	}
	var fixedTag, commitSHA string
	if err := db.QueryRow(`SELECT rel.tag, rel.commit_sha FROM lingo_tickets t JOIN lingo_releases rel ON rel.id = t.fixed_release_id WHERE t.id = $1`, byCommit).
		Scan(&fixedTag, &commitSHA); err != nil || fixedTag != "v1.1.0" || commitSHA != "tagsha110" {
		t.Errorf("fixed release = %s %s %v", fixedTag, commitSHA, err)
	}
	var links int
	if err := db.QueryRow("SELECT count(*) FROM lingo_release_tickets").Scan(&links); err != nil || links != 3 {
		t.Errorf("release_tickets = %d %v", links, err) // rc 关联 #13，正式版关联 #12 #13
	}

	// 上线：部署工作流成功（head_branch 为 tag）
	send("w1", "workflow_run", `{"action":"completed","workflow_run":{"id":5,"run_attempt":1,"path":".gitea/workflows/deploy-prod.yml@refs/tags/v1.1.0",
		"conclusion":"success","head_branch":"v1.1.0","head_sha":"tagsha110","html_url":"http://g/run/5","completed_at":"2026-11-20T01:00:00Z"},`+repo+`}`)
	evs = rec.waitFor(t, 4)
	for _, e := range evs[2:] {
		if e.Kind != lingo.EventTicketDeployed || e.Status != "deployed" || e.PrevStatus != "released" {
			t.Errorf("deploy event = %+v", e)
		}
	}
	var deployed int
	if err := db.QueryRow("SELECT count(*) FROM lingo_tickets WHERE status = 'deployed' AND deployed_at IS NOT NULL").Scan(&deployed); err != nil || deployed != 2 {
		t.Errorf("deployed = %d %v", deployed, err)
	}
	// 重复的上线信号（提交状态、手动）幂等
	send("s1", "status", `{"id":3,"sha":"tagsha110","context":"deploy/production","state":"success","created_at":"2026-11-20T02:00:00Z",`+repo+`}`)
	if err := l.MarkDeployed(ctx, "demo/api", "v1.1.0", "production"); err != nil {
		t.Errorf("MarkDeployed: %v", err)
	}
	waitProcessed(t, db, 4)
	if got := rec.waitFor(t, 4); len(got) != 4 {
		t.Errorf("duplicate deploy events: %d", len(got))
	}
	// 版本已上线后才关联上的工单（如 Release 更新时补上）：发布即上线
	if _, err := db.Exec("UPDATE lingo_tickets SET fixed_commit = 'abc123' WHERE id = $1", untouched); err != nil {
		t.Fatal(err)
	}
	send("r2", "release", `{"action":"updated","release":{"id":10,"tag_name":"v1.1.0","name":"1.1","body":"修复 #13","prerelease":false,
		"published_at":"2026-11-20T00:00:00Z","html_url":"http://g/demo/api/releases/v1.1.0"},`+repo+`}`)
	evs = rec.waitFor(t, 6)
	if evs[4].Kind != lingo.EventTicketReleased || evs[5].Kind != lingo.EventTicketDeployed || evs[5].TicketID != untouched {
		t.Errorf("late link events = %+v", evs[4:])
	}

	// 跨仓库修复：后端工单由前端仓库的 PR 修复，随前端版本发布
	var cross int64
	if err := db.QueryRow(`INSERT INTO lingo_tickets (user_id, project_id, repo_id, type, title, status, gitea_issue_number, fixed_commit, fixed_repo)
		SELECT 'u1', project_id, id, 'bug', 't', 'fixed', 15, 'webfix', 'demo/web' FROM lingo_repos WHERE name = 'api' RETURNING id`).Scan(&cross); err != nil {
		t.Fatal(err)
	}
	send("r3", "release", `{"action":"published","release":{"id":20,"tag_name":"v2.0.0","name":"web 2","body":"","prerelease":false,
		"published_at":"2026-11-20T03:00:00Z","html_url":"http://g/demo/web/releases/v2.0.0"},"repository":{"full_name":"demo/web"},"sender":{"login":"dev"}}`)
	evs = rec.waitFor(t, 7)
	if evs[6].TicketID != cross || evs[6].Kind != lingo.EventTicketReleased {
		t.Errorf("cross-repo release event = %+v", evs[6])
	}
}
