package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// REQ-005 6.1 / 6.2 中回归用例未覆盖的验证项。

// 更换 Gitea token 后，不重启服务，新建的 Issue 由新 token 创建。
func TestSettingsTokenRotation(t *testing.T) {
	db := testDB(t, true)
	g := &giteaServer{templates: "[]"}
	var mu sync.Mutex
	var auth []string // 每次建 Issue 请求的 Authorization
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues") {
			mu.Lock()
			auth = append(auth, r.Header.Get("Authorization"))
			mu.Unlock()
		}
		g.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	l := newBareLingo(t, db, nil, nil)
	ctx := context.Background()
	if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{
		BaseURL: sptr(srv.URL), Token: sptr("token-old")}}); err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	create := func() int64 {
		tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "问题 " + time.Now().String()})
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if got, err := l.GetTicket(ctx, alice, tk.ID); err == nil && got.Status == "open" {
				return tk.ID
			}
			if time.Now().After(deadline) {
				t.Fatalf("ticket %d not forwarded", tk.ID)
			}
		}
	}
	create()
	if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{Token: sptr("token-new")}}); err != nil {
		t.Fatal(err)
	}
	create()
	mu.Lock()
	defer mu.Unlock()
	if len(auth) != 2 || auth[0] != "token token-old" || auth[1] != "token token-new" {
		t.Errorf("issue requests authorized with %q", auth)
	}
}

// 开启自动注册时，修改 Webhook 密钥后补跑自检，把新密钥同步到 Gitea 的 Webhook。
func TestSettingsWebhookSecretSyncedToGitea(t *testing.T) {
	db := testDB(t, true)
	var mu sync.Mutex
	var hooks []map[string]any
	var secrets []string // 创建或修改 Webhook 时写入的密钥
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		record := func(h map[string]any) {
			if c, ok := h["config"].(map[string]any); ok {
				if s, ok := c["secret"].(string); ok {
					secrets = append(secrets, s)
				}
			}
		}
		switch {
		case p == "/user":
			_, _ = w.Write([]byte(`{"id":1,"login":"lingo-bot"}`))
		case p == "/version":
			_, _ = w.Write([]byte(`{"version":"1.27.3"}`))
		case p == "/repos/demo/api":
			_, _ = w.Write([]byte(`{"id":1,"name":"api","full_name":"demo/api","has_issues":true}`))
		case p == "/repos/demo/api/collaborators/lingo-bot/permission":
			_, _ = w.Write([]byte(`{"permission":"write"}`))
		case p == "/repos/demo/api/labels":
			_, _ = w.Write([]byte(`[]`))
		case p == "/repos/demo/api/hooks" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(hooks)
		case p == "/repos/demo/api/hooks" && r.Method == "POST":
			var h map[string]any
			_ = json.NewDecoder(r.Body).Decode(&h)
			record(h)
			h["id"] = 9
			hooks = append(hooks, h)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(h)
		case p == "/repos/demo/api/hooks/9" && r.Method == "PATCH":
			var h map[string]any
			_ = json.NewDecoder(r.Body).Decode(&h)
			record(h)
			_ = json.NewEncoder(w).Encode(hooks[0])
		case strings.HasSuffix(p, "/tests"):
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.Close)
	l := newBareLingo(t, db, nil, func(c *lingo.Config) { c.SkipSelfCheck = false })
	ctx := context.Background()
	if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{
		BaseURL: sptr(g.URL), Token: sptr("t"), WebhookSecret: sptr("secret-a"), AutoRegisterWebhook: bptr(true),
		WebhookURL: sptr("https://lingo.example.com/webhooks/gitea")}}); err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(secrets) == 0 {
			return ""
		}
		return secrets[len(secrets)-1]
	}
	wait := func(want string) {
		for deadline := time.Now().Add(40 * time.Second); last() != want; time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("webhook secret in gitea = %q, want %q", last(), want)
			}
		}
	}
	wait("secret-a") // 启动自检注册 Webhook
	if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{WebhookSecret: sptr("secret-b")}}); err != nil {
		t.Fatal(err)
	}
	wait("secret-b") // 保存后补跑自检，同步新密钥
}

// PR #36 审查：数据库中的 Gitea 配置不可用时，保留原连接与原地址（Issue 链接不变），版本不前进。
func TestSettingsRejectedGiteaKeepsPrevious(t *testing.T) {
	db := testDB(t, true)
	gsrv := httptest.NewServer(&giteaServer{templates: "[]"})
	t.Cleanup(gsrv.Close)
	l := newBareLingo(t, db, nil, nil)
	ctx := context.Background()
	if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{
		BaseURL: sptr(gsrv.URL), Token: sptr("t")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ListTriage(ctx, rita, lingo.TriageFilter{}); err != nil { // 同步项目入库
		t.Fatal(err)
	}
	id := seedTicketRow(t, db, "u1", "", "api", "bug", "x", "open", 12)
	before, _ := l.GetTicket(ctx, alice, id)
	// 绕过接口校验直接写入不可用的地址（如较旧版本保存的值）
	if _, err := db.Exec(`UPDATE lingo_settings SET content = jsonb_set(content, '{base_url}', '"not a url"'), version = version + 1 WHERE category = 'gitea'`); err != nil {
		t.Fatal(err)
	}
	s, err := l.ConnectionSettings(ctx, admin) // 触发刷新
	if err != nil {
		t.Fatal(err)
	}
	after, _ := l.GetTicket(ctx, alice, id)
	if before.IssueURL == "" || after.IssueURL != before.IssueURL {
		t.Errorf("issue url changed: %q -> %q", before.IssueURL, after.IssueURL)
	}
	if !strings.HasPrefix(after.IssueURL, gsrv.URL) || s.Gitea.Version != 2 || s.Gitea.Applied {
		t.Errorf("view version = %d applied=%v, url %q", s.Gitea.Version, s.Gitea.Applied, after.IssueURL)
	}
}

// PR #36 审查：配置行缺失时保存会补建，不会静默丢失。
func TestSettingsMissingRowRecreated(t *testing.T) {
	db := testDB(t, true)
	gsrv := httptest.NewServer(&giteaServer{templates: "[]"})
	t.Cleanup(gsrv.Close)
	l := newBareLingo(t, db, nil, nil)
	other := newBareLingo(t, db, nil, nil)
	ctx := context.Background()
	// 先保存几次，版本前进到 3，两个实例都已应用
	for _, s := range []string{"a", "b", "c"} {
		if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{
			BaseURL: sptr(gsrv.URL), Token: sptr("t"), WebhookSecret: sptr(s)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := other.ConnectionSettings(ctx, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM lingo_settings WHERE category = 'gitea'`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	s, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{
		BaseURL: sptr(gsrv.URL), Token: sptr("t"), WebhookSecret: sptr("fresh")}})
	if err != nil || s.Gitea.Version != 1 || !s.Gitea.Applied {
		t.Fatalf("recreated = %+v %v", s.Gitea, err)
	}
	// 补建后版本回到 1：保存的实例与其他实例都按修改时间判断为更新，改用新密钥
	if s, err := other.ConnectionSettings(ctx, admin); err != nil || !s.Gitea.Applied {
		t.Fatalf("other = %+v %v", s.Gitea, err)
	}
	for name, h := range map[string]http.Handler{"saver": l.WebhookHandler(), "other": other.WebhookHandler()} {
		if w := deliver(h, "d-"+name, "issue_comment", commentPayload, "fresh"); w.Code != http.StatusOK {
			t.Errorf("%s with new secret: %d", name, w.Code)
		}
	}
}

// PR #36 审查：代码中开启了自动注册而密钥未配置时，只改模型的保存与测试不受影响。
func TestSettingsModelSaveIgnoresGiteaAutoRegister(t *testing.T) {
	db := testDB(t, true)
	l := newBareLingo(t, db, nil, func(c *lingo.Config) { c.Gitea.AutoRegisterWebhook = true })
	ctx := context.Background()
	in := lingo.ConnectionSettingsUpdate{Model: &lingo.ModelConnectionUpdate{Main: &lingo.ModelSlotUpdate{Provider: sptr("deepseek"), APIKey: sptr("k")}}}
	if s, err := l.UpdateConnectionSettings(ctx, admin, in); err != nil || !s.Model.Configured || !s.Model.Applied || s.Model.Main.Model != "deepseek-chat" {
		t.Errorf("model save = %+v %v", s.Model, err)
	}
	if _, err := l.TestConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{}); err != nil {
		t.Errorf("empty test: %v", err)
	}
}

// 迁移 0004 能在已执行 0001–0003 的库上增量执行。
func TestSettingsMigrationIncremental(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP TABLE lingo_settings; DELETE FROM lingo_schema_migrations WHERE name LIKE '0004_%'`); err != nil {
		t.Fatal(err)
	}
	if err := lingo.Migrate(ctx, db); err != nil {
		t.Fatalf("incremental Migrate: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM lingo_settings`).Scan(&n); err != nil || n != 2 {
		t.Errorf("settings rows = %d %v", n, err)
	}
	if st, err := lingo.CheckMigrations(ctx, db); err != nil || !st.Ready {
		t.Errorf("after incremental: %+v %v", st, err)
	}
}
