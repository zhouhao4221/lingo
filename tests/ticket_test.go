package tests

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// giteaServer 模拟建单用到的 Gitea 接口：/user、标签、模板、列出与创建 Issue。
type giteaServer struct {
	mu        sync.Mutex
	labels    []map[string]any
	issues    []map[string]any
	templates string
	created   []map[string]any // POST /issues 的请求体
	createErr int              // 非 0 时 POST /issues 返回该状态码
}

func (g *giteaServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case p == "/user":
		_, _ = w.Write([]byte(`{"id":1,"login":"lingo-bot"}`))
	case p == "/repos/demo/api/labels" && r.Method == "GET":
		_ = json.NewEncoder(w).Encode(g.labels)
	case p == "/repos/demo/api/labels" && r.Method == "POST":
		var l map[string]any
		_ = json.NewDecoder(r.Body).Decode(&l)
		l["id"] = len(g.labels) + 100
		g.labels = append(g.labels, l)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(l)
	case p == "/repos/demo/api/issue_templates":
		_, _ = w.Write([]byte(g.templates))
	case p == "/repos/demo/api/issues" && r.Method == "GET":
		_ = json.NewEncoder(w).Encode(g.issues)
	case p == "/repos/demo/api/issues" && r.Method == "POST" && g.createErr != 0:
		http.Error(w, `{"message":"forbidden"}`, g.createErr)
	case p == "/repos/demo/api/issues" && r.Method == "POST":
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		g.created = append(g.created, req)
		is := map[string]any{"id": 1000 + len(g.issues), "number": 40 + len(g.issues), "title": req["title"], "body": req["body"],
			"state": "open", "user": map[string]any{"login": "lingo-bot"}}
		g.issues = append(g.issues, is)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(is)
	default:
		http.NotFound(w, r)
	}
}

const bugTemplate = `[{"name":"Bug","title":"[Bug]: ","labels":["type/bug"],"file_name":"bug_report.yaml","body":[
  {"type":"textarea","id":"what","attributes":{"label":"发生了什么"},"validations":{"required":true}},
  {"type":"input","id":"version","attributes":{"label":"版本"}}]}]`

func newTicketLingo(t *testing.T, g *giteaServer, notifier lingo.Notifier, auto map[lingo.TicketType]bool) (*lingo.Lingo, *sql.DB, string) {
	t.Helper()
	db := testDB(t, true)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{}, Notifier: notifier,
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		Projects: []lingo.Project{{Key: "demo", Name: "Demo", AutoForward: auto, Repos: []lingo.Repo{
			{Owner: "demo", Name: "api", Kind: lingo.RepoBackend, Assignees: []string{"dev-agent"}, Templates: map[string]string{"bug": "bug_report.yaml"}},
			{Owner: "demo", Name: "web", Kind: lingo.RepoFrontend}}}},
		Authenticate: func(*http.Request) (lingo.User, error) { return lingo.User{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return l, db, srv.URL
}

func runLingo(t *testing.T, l *lingo.Lingo) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func waitTicket(t *testing.T, l *lingo.Lingo, u lingo.User, id int64, status string) lingo.Ticket {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		tk, err := l.GetTicket(context.Background(), u, id)
		if err == nil && tk.Status == status {
			return tk
		}
		if time.Now().After(deadline) {
			t.Fatalf("ticket %d: %+v %v, want status %s", id, tk, err, status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var (
	alice = lingo.User{ID: "u1", Name: "Alice", Email: "alice@example.com", Role: lingo.RoleExternal}
	rita  = lingo.User{ID: "r1", Name: "Rita", Role: lingo.RoleReviewer}
)

func TestCreateTicketAutoForwardCreatesIssue(t *testing.T) {
	g := &giteaServer{templates: bugTemplate}
	rec := &recorder{}
	l, db, base := newTicketLingo(t, g, rec, map[lingo.TicketType]bool{lingo.TicketBug: true})
	ctx := context.Background()
	tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "补充：只在 Safari 出现",
		Fields: map[string][]string{"what": {"登录后白屏"}, "version": {"2.3.1"}}, Context: map[string]string{"page": "/login"}, Version: "2.3.1"})
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != "submitted" || tk.Repo != "demo/api" || tk.ProjectKey != "demo" || tk.Title != "补充：只在 Safari 出现" {
		t.Errorf("created = %+v", tk)
	}
	runLingo(t, l)
	got := waitTicket(t, l, alice, tk.ID, "open")
	if got.IssueURL != base+"/demo/api/issues/40" {
		t.Errorf("issue url = %s", got.IssueURL)
	}
	g.mu.Lock()
	req := g.created[0]
	g.mu.Unlock()
	body, _ := req["body"].(string)
	for _, want := range []string{"### 发生了什么\n\n登录后白屏", "### 版本\n\n2.3.1", "### 补充说明", "- 项目：demo", "- page：/login", "<!-- lingo:ticket="} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Alice") || strings.Contains(body, "alice@example.com") {
		t.Errorf("issue body leaks submitter identity:\n%s", body)
	}
	if req["title"] != "[Bug]: 补充：只在 Safari 出现" || len(req["labels"].([]any)) != 2 || req["assignees"].([]any)[0] != "dev-agent" {
		t.Errorf("create request = %+v", req)
	}
	// 事件：ticket.created，submitted → open
	evs := rec.waitFor(t, 2)
	if evs[0].Kind != lingo.EventTicketCreated || evs[1].Kind != lingo.EventTicketStatus || evs[1].Status != "open" || evs[1].URL == "" {
		t.Errorf("events = %+v", evs)
	}
	var tasks int
	if err := db.QueryRow("SELECT count(*) FROM outbox WHERE kind = 'gitea' AND status = 'done'").Scan(&tasks); err != nil || tasks != 1 {
		t.Errorf("done tasks = %d %v", tasks, err)
	}
	// 数据隔离：他人看不到
	if _, err := l.GetTicket(ctx, lingo.User{ID: "u2", Role: lingo.RoleExternal}, tk.ID); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("other user: %v", err)
	}
}

func TestCreateIssueDedupByMarker(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	l, db, _ := newTicketLingo(t, g, nil, map[lingo.TicketType]bool{lingo.TicketBug: true})
	tk, err := l.CreateTicket(context.Background(), alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "崩溃"})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟上次已建好 Issue 但没保存：任务此前被领取过，Gitea 中已有带标记的机器人 Issue
	if _, err := db.Exec("UPDATE outbox SET attempts = 1 WHERE kind = 'gitea'"); err != nil {
		t.Fatal(err)
	}
	g.issues = []map[string]any{{"id": 7, "number": 77, "state": "open", "user": map[string]any{"login": "lingo-bot"},
		"body": "x\n<!-- lingo:ticket=" + jsonNum(tk.ID) + " -->"}}
	runLingo(t, l)
	got := waitTicket(t, l, alice, tk.ID, "open")
	if !strings.HasSuffix(got.IssueURL, "/issues/77") || len(g.created) != 0 {
		t.Errorf("dedup failed: %+v created %d", got, len(g.created))
	}
}

func TestCreateIssuePermanentFailureMarksAbnormal(t *testing.T) {
	g := &giteaServer{templates: "[]", createErr: http.StatusForbidden}
	rec := &recorder{}
	l, db, _ := newTicketLingo(t, g, rec, map[lingo.TicketType]bool{lingo.TicketBug: true})
	tk, err := l.CreateTicket(context.Background(), alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	waitTicket(t, l, alice, tk.ID, "abnormal")
	var status string
	if err := db.QueryRow("SELECT status FROM outbox WHERE kind = 'gitea'").Scan(&status); err != nil || status != "failed" {
		t.Errorf("task status = %s %v", status, err)
	}
	if evs := rec.waitFor(t, 2); evs[1].Status != "abnormal" || evs[1].PrevStatus != "submitted" {
		t.Errorf("events = %+v", evs)
	}
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestTriageForwardFlow(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	rec := &recorder{}
	l, _, _ := newTicketLingo(t, g, rec, nil)
	ctx := context.Background()
	tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketFeature, Content: "希望支持导出 Excel"})
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != "pending_triage" || tk.TriageSince == nil {
		t.Fatalf("created = %+v", tk)
	}
	if _, err := l.ForwardTicket(ctx, alice, tk.ID, lingo.ForwardOptions{}); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("external forward: %v", err)
	}
	if _, err := l.ForwardTicket(ctx, rita, tk.ID, lingo.ForwardOptions{Repo: "nope/x"}); !errors.Is(err, lingo.ErrInvalidInput) {
		t.Errorf("bad repo: %v", err)
	}
	if _, err := l.ForwardTicket(ctx, rita, tk.ID, lingo.ForwardOptions{Title: "导出 Excel", Note: "下个迭代"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ForwardTicket(ctx, rita, tk.ID, lingo.ForwardOptions{}); !errors.Is(err, lingo.ErrConflict) {
		t.Errorf("forward twice: %v", err)
	}
	runLingo(t, l)
	waitTicket(t, l, rita, tk.ID, "open")
	g.mu.Lock()
	body, _ := g.created[0]["body"].(string)
	title := g.created[0]["title"]
	g.mu.Unlock()
	if title != "导出 Excel" || !strings.Contains(body, "- 由 Rita 确认转出") || !strings.Contains(body, "> 下个迭代") {
		t.Errorf("issue = %v\n%s", title, body)
	}
	evs := rec.waitFor(t, 5)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, string(e.Kind))
	}
	// created、triage.required、status(pending→submitted)、triage.resolved、status(submitted→open)
	if strings.Join(kinds, ",") != "ticket.created,triage.required,ticket.status,triage.resolved,ticket.status" ||
		evs[3].Action != lingo.TriageForward || evs[3].ActorID != "r1" {
		t.Errorf("events = %v %+v", kinds, evs[3])
	}
}

func TestSetAutoForwardAndProjectSync(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	l, db, _ := newTicketLingo(t, g, nil, nil)
	ctx := context.Background()
	admin := lingo.User{ID: "root", Role: lingo.RoleAdmin}
	if err := l.SetAutoForward(ctx, rita, "demo", lingo.TicketBug, true); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("reviewer: %v", err)
	}
	if err := l.SetAutoForward(ctx, admin, "demo", lingo.TicketBug, true); err != nil {
		t.Fatal(err)
	}
	if tk, _ := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketBug, Repo: "demo/web", Content: "x"}); tk.Status != "submitted" || tk.Repo != "demo/web" {
		t.Errorf("after override: %+v", tk)
	}
	if err := l.ResetAutoForward(ctx, admin, "demo", lingo.TicketBug); err != nil {
		t.Fatal(err)
	}
	if tk, _ := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "y"}); tk.Status != "pending_triage" {
		t.Errorf("after reset: %+v", tk)
	}
	var repos, def int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM repos WHERE enabled),
		(SELECT count(*) FROM projects p JOIN repos r ON r.id = p.default_repo_id WHERE p.key = 'demo' AND r.name = 'api')`).Scan(&repos, &def); err != nil {
		t.Fatal(err)
	}
	if repos != 2 || def != 1 {
		t.Errorf("synced repos %d default %d", repos, def)
	}
}
