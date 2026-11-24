package tests

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

func sptr(s string) *string { return &s }
func bptr(b bool) *bool     { return &b }

// openAIServer 是 OpenAI 兼容接口的替身：所有对话都返回 out，可要求固定的 API Key。
func openAIServer(t *testing.T, out, key string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key != "" && r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, `{"error":{"message":"bad key sk-leak"}}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "m", "choices": []map[string]any{
			{"message": map[string]string{"content": out}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// newBareLingo 创建只传数据库、项目与身份的实例（连接配置全靠数据库）。
func newBareLingo(t *testing.T, db *sql.DB, logs *logBuf, mut func(*lingo.Config)) *lingo.Lingo {
	t.Helper()
	cfg := lingo.Config{DB: db, SkipSelfCheck: true, ReconcileInterval: -1,
		Projects: []lingo.Project{{Key: "demo", Name: "Demo", AutoForward: map[lingo.TicketType]bool{lingo.TicketBug: true},
			Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate: headerUser}
	if logs != nil {
		cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}
	if mut != nil {
		mut(&cfg)
	}
	l, err := lingo.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSettingsUnconfiguredStartAndRecovery(t *testing.T) {
	db := testDB(t, true)
	g := &giteaServer{templates: "[]"}
	gsrv := httptest.NewServer(g)
	t.Cleanup(gsrv.Close)
	logs := &logBuf{}
	l := newBareLingo(t, db, logs, nil)
	ctx := context.Background()
	runLingo(t, l)

	// 未配置：Webhook 503，问答 ErrNoModel，提交的 Bug 建单任务不领取、不计失败
	if w := deliver(l.WebhookHandler(), "d-1", "issue_comment", commentPayload, "s"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("webhook unconfigured: %d", w.Code)
	}
	conv, _ := l.CreateConversation(ctx, alice, "")
	if _, err := l.SendMessage(ctx, alice, conv.ID, lingo.MessageInput{Content: "hi"}); !errors.Is(err, lingo.ErrNoModel) {
		t.Errorf("model unconfigured: %v", err)
	}
	tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "登录页白屏"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	var failures, attempts int
	if err := db.QueryRow(`SELECT failures, attempts FROM lingo_outbox WHERE ticket_id = $1 AND kind = 'gitea'`, tk.ID).Scan(&failures, &attempts); err != nil {
		t.Fatal(err)
	}
	if failures != 0 || attempts != 0 {
		t.Errorf("unconfigured task failures=%d attempts=%d", failures, attempts)
	}
	if cs := l.SelfCheck(ctx); len(cs) != 1 || cs[0].Item != "config" {
		t.Errorf("selfcheck = %+v", cs)
	}

	// 配置 Gitea 与模型：不重启即恢复
	ai := openAIServer(t, `{"intent":"out_of_scope"}`, "sk-1234567890abcd")
	s, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{
		Gitea: &lingo.GiteaConnectionUpdate{BaseURL: sptr(gsrv.URL), Token: sptr("gitea-token-1234567890"), WebhookSecret: sptr("s")},
		Model: &lingo.ModelConnectionUpdate{Main: &lingo.ModelSlotUpdate{Provider: sptr("openai"), BaseURL: sptr(ai.URL), APIKey: sptr("sk-1234567890abcd"), Model: sptr("m")}},
	})
	if err != nil || !s.Configured || s.Gitea.Token.Masked != "git****7890" || s.Model.Main.APIKey.Masked != "****" || s.Gitea.Version != 1 {
		t.Fatalf("update = %+v %v", s, err)
	}
	if b, _ := json.Marshal(s); strings.Contains(string(b), "gitea-token-1234567890") || strings.Contains(string(b), "sk-1234567890abcd") {
		t.Errorf("view leaks secrets: %s", b)
	}
	// 依赖机器人账号的任务可能被推迟一次（账号刚开始获取），给足时间
	for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if got, err := l.GetTicket(ctx, alice, tk.ID); err == nil && got.Status == "open" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("issue not created after configuring gitea")
		}
	}
	if w := deliver(l.WebhookHandler(), "d-2", "issue_comment", commentPayload, "s"); w.Code != http.StatusOK {
		t.Errorf("webhook configured: %d %s", w.Code, w.Body)
	}
	if m, err := l.SendMessage(ctx, alice, conv.ID, lingo.MessageInput{Content: "这个功能怎么用"}); err != nil || m.Status != "done" {
		t.Errorf("assistant after config: %+v %v", m, err)
	}
	// 审计日志只含字段名
	if out := logs.String(); !strings.Contains(out, "connection settings updated") || strings.Contains(out, "gitea-token-1234567890") || strings.Contains(out, "sk-1234567890abcd") {
		t.Errorf("logs:\n%s", out)
	}
}

func TestSettingsRulesAndPermissions(t *testing.T) {
	db := testDB(t, true)
	gsrv := httptest.NewServer(&giteaServer{templates: "[]"})
	t.Cleanup(gsrv.Close)
	ctx := context.Background()
	// 代码中配置了 Gitea 地址：该项来源为 config，不能修改；未配置回调时只有 admin 能看能改
	l := newBareLingo(t, db, nil, func(c *lingo.Config) { c.Gitea.BaseURL = gsrv.URL })
	s, err := l.ConnectionSettings(ctx, admin)
	if err != nil || s.Gitea.BaseURL.Source != lingo.SourceConfig || s.Gitea.Token.Source != lingo.SourceUnset || s.Gitea.Configured {
		t.Fatalf("view = %+v %v", s.Gitea, err)
	}
	if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{BaseURL: sptr("https://x.example.com")}}); !errors.Is(err, lingo.ErrConflict) {
		t.Errorf("config field: %v", err)
	}
	for _, u := range []lingo.User{alice, rita, agent} {
		if _, err := l.ConnectionSettings(ctx, u); !errors.Is(err, lingo.ErrForbidden) {
			t.Errorf("%s view: %v", u.Role, err)
		}
	}
	s, err = l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{Token: sptr("tok-abcdefghijkl")}})
	if err != nil || !s.Gitea.Configured || s.Gitea.Token.Source != lingo.SourceDatabase {
		t.Errorf("token = %+v %v", s.Gitea, err)
	}
	// 改模型地址而不重填 API Key：拒绝；格式不合法：拒绝
	if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Model: &lingo.ModelConnectionUpdate{Main: &lingo.ModelSlotUpdate{
		Provider: sptr("openai"), Model: sptr("m"), APIKey: sptr("sk-aaaaaaaaaaaa")}}}); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]lingo.ConnectionSettingsUpdate{
		"host change": {Model: &lingo.ModelConnectionUpdate{Main: &lingo.ModelSlotUpdate{BaseURL: sptr("https://evil.example.com")}}},
		"bad url":     {Model: &lingo.ModelConnectionUpdate{Main: &lingo.ModelSlotUpdate{BaseURL: sptr("not a url"), APIKey: sptr("k")}}},
		"provider":    {Model: &lingo.ModelConnectionUpdate{Main: &lingo.ModelSlotUpdate{Provider: sptr("claude")}}},
		"autoreg":     {Gitea: &lingo.GiteaConnectionUpdate{AutoRegisterWebhook: bptr(true)}},
	} {
		if _, err := l.UpdateConnectionSettings(ctx, admin, in); !errors.Is(err, lingo.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// 回调放行 reviewer 查看
	l2 := newBareLingo(t, db, nil, func(c *lingo.Config) {
		c.AuthorizeSettings = func(_ context.Context, u lingo.User, write bool) error {
			if u.Role == lingo.RoleReviewer && !write {
				return nil
			}
			return lingo.ErrForbidden
		}
	})
	if _, err := l2.ConnectionSettings(ctx, rita); err != nil {
		t.Errorf("reviewer via callback: %v", err)
	}
	if _, err := l2.UpdateConnectionSettings(ctx, rita, lingo.ConnectionSettingsUpdate{}); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("reviewer write: %v", err)
	}
	if _, err := l2.ConnectionSettings(ctx, admin); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("admin denied by callback: %v", err)
	}
}

func TestSettingsTestConnectionAndOtherInstance(t *testing.T) {
	db := testDB(t, true)
	g := &giteaServer{templates: "[]"}
	gsrv := httptest.NewServer(g)
	t.Cleanup(gsrv.Close)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"token gitea-secret-0000 invalid"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(bad.Close)
	ctx := context.Background()
	l1 := newBareLingo(t, db, nil, nil)
	l2 := newBareLingo(t, db, nil, nil)

	// 连通测试：不保存；失败原因已脱敏
	ai := openAIServer(t, "pong", "sk-right-key-123456")
	res, err := l1.TestConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{
		Gitea: &lingo.GiteaConnectionUpdate{BaseURL: sptr(bad.URL), Token: sptr("gitea-secret-0000")},
		Model: &lingo.ModelConnectionUpdate{Main: &lingo.ModelSlotUpdate{Provider: sptr("openai"), BaseURL: sptr(ai.URL), APIKey: sptr("sk-wrong-key-00000"), Model: sptr("m")}},
	})
	if err != nil || res.Gitea.OK || res.Gitea.Code != "auth_failed" || res.Model.Code != "auth_failed" {
		t.Fatalf("test = %+v %v", res, err)
	}
	if b, _ := json.Marshal(res); strings.Contains(string(b), "gitea-secret-0000") || strings.Contains(string(b), "sk-leak") || strings.Contains(string(b), bad.URL) {
		t.Errorf("test result leaks: %s", b)
	}
	if s, _ := l1.ConnectionSettings(ctx, admin); s.Gitea.Version != 0 {
		t.Errorf("test saved settings: %+v", s.Gitea)
	}
	res, err = l1.TestConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{
		Gitea: &lingo.GiteaConnectionUpdate{BaseURL: sptr(gsrv.URL), Token: sptr("t")},
		Model: &lingo.ModelConnectionUpdate{Main: &lingo.ModelSlotUpdate{Provider: sptr("openai"), BaseURL: sptr(ai.URL), APIKey: sptr("sk-right-key-123456"), Model: sptr("m")}},
	})
	if err != nil || !res.Gitea.OK || res.Gitea.Login != "lingo-bot" || !res.Model.OK {
		t.Errorf("test ok = %+v %v", res, err)
	}

	// 实例 1 保存后，实例 2 查看配置即生效；改 Webhook 密钥后按新密钥验签
	if _, err := l1.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{
		Gitea: &lingo.GiteaConnectionUpdate{BaseURL: sptr(gsrv.URL), Token: sptr("t"), WebhookSecret: sptr("old")}}); err != nil {
		t.Fatal(err)
	}
	if s, err := l2.ConnectionSettings(ctx, admin); err != nil || !s.Gitea.Configured {
		t.Fatalf("instance 2 = %+v %v", s.Gitea, err)
	}
	if w := deliver(l2.WebhookHandler(), "d-1", "issue_comment", commentPayload, "old"); w.Code != http.StatusOK {
		t.Errorf("instance 2 webhook: %d %s", w.Code, w.Body)
	}
	if _, err := l1.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Gitea: &lingo.GiteaConnectionUpdate{WebhookSecret: sptr("new")}}); err != nil {
		t.Fatal(err)
	}
	if w := deliver(l1.WebhookHandler(), "d-2", "issue_comment", commentPayload, "old"); w.Code != http.StatusUnauthorized {
		t.Errorf("old secret: %d", w.Code)
	}
	if w := deliver(l1.WebhookHandler(), "d-3", "issue_comment", commentPayload, "new"); w.Code != http.StatusOK {
		t.Errorf("new secret: %d %s", w.Code, w.Body)
	}

	// REST：admin 可用，其他角色 403
	rc := restClient{t: t, h: l1.Handler()}
	if code, obj, _ := rc.do("root", "admin", "GET", "/settings/connections", nil); code != 200 || obj["gitea"] == nil {
		t.Errorf("rest get: %d %v", code, obj)
	}
	if code, _, _ := rc.do("u1", "external", "GET", "/settings/connections", nil); code != http.StatusForbidden {
		t.Errorf("rest external: %d", code)
	}
	if code, obj, _ := rc.do("root", "admin", "PUT", "/settings/connections", map[string]any{"gitea": map[string]any{"base_url": "https://evil.example.com"}}); code != http.StatusBadRequest {
		t.Errorf("rest host change: %d %v", code, obj)
	}
	if code, obj, _ := rc.do("root", "admin", "POST", "/settings/connections/test", nil); code != 200 || obj["gitea"] == nil {
		t.Errorf("rest test: %d %v", code, obj)
	}
}
