package tests

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zhouhao4221/lingo"
	"github.com/zhouhao4221/lingo/internal/gitea"
)

func TestSelfCheckRegistersWebhookAndConfirmsDelivery(t *testing.T) {
	db := testDB(t, true)
	var mu sync.Mutex
	var hooks []map[string]any
	var labels []string
	var lingoWebhook http.Handler
	var whURL string
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch {
		case p == "/user":
			_, _ = w.Write([]byte(`{"id":1,"login":"lingo-bot"}`))
		case p == "/version":
			_, _ = w.Write([]byte(`{"version":"1.27.3"}`))
		case p == "/settings/attachment":
			_, _ = w.Write([]byte(`{"enabled":true,"max_size":4,"allowed_types":".png,.log"}`))
		case p == "/repos/demo/api":
			_, _ = w.Write([]byte(`{"id":1,"name":"api","full_name":"demo/api","has_issues":true}`))
		case p == "/repos/demo/api/collaborators/lingo-bot/permission":
			_, _ = w.Write([]byte(`{"permission":"write"}`))
		case p == "/repos/demo/api/labels" && r.Method == "GET":
			_, _ = w.Write([]byte(`[]`))
		case p == "/repos/demo/api/labels" && r.Method == "POST":
			var l map[string]any
			_ = json.NewDecoder(r.Body).Decode(&l)
			labels = append(labels, l["name"].(string))
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(labels), "name": l["name"]})
		case p == "/repos/demo/api/hooks" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(hooks)
		case p == "/repos/demo/api/hooks" && r.Method == "POST":
			var h map[string]any
			_ = json.NewDecoder(r.Body).Decode(&h)
			h["id"] = 9
			hooks = append(hooks, h)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(h)
		case p == "/repos/demo/api/hooks/9/tests":
			// 模拟 Gitea 的测试投递：以 Hook 配置的密钥签名一个 push 事件发给灵问
			secret := hooks[0]["config"].(map[string]any)["secret"].(string)
			body := `{"ref":"refs/heads/main","repository":{"full_name":"demo/api"},"sender":{"login":"lingo-bot"}}`
			req := httptest.NewRequest("POST", whURL, strings.NewReader(body))
			req.Header.Set(gitea.HeaderDelivery, "test-1")
			req.Header.Set(gitea.HeaderEvent, "push")
			req.Header.Set(gitea.HeaderEventType, "push")
			req.Header.Set(gitea.HeaderSignature, hex.EncodeToString(gitea.Sign(secret, []byte(body))))
			rec := httptest.NewRecorder()
			lingoWebhook.ServeHTTP(rec, req)
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.Close)
	whURL = "https://lingo.example.com/webhooks/gitea"
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{},
		Gitea:         lingo.GiteaConfig{BaseURL: g.URL, Token: "t", WebhookSecret: "s3cret", AutoRegisterWebhook: true, WebhookURL: whURL},
		Projects:      []lingo.Project{{Key: "demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate:  func(*http.Request) (lingo.User, error) { return lingo.User{}, nil },
		SkipSelfCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lingoWebhook = l.WebhookHandler()
	rs := l.SelfCheck(context.Background())
	got := map[string]lingo.CheckResult{}
	for _, r := range rs {
		got[r.Repo+"|"+r.Item] = r
		if r.Level != "ok" {
			t.Errorf("%s %s: %s %s", r.Repo, r.Item, r.Level, r.Message)
		}
	}
	if got["demo/api|webhook"].Message != "webhook created" || got["demo/api|delivery"].Level != "ok" {
		t.Errorf("results = %+v", rs)
	}
	mu.Lock()
	if len(hooks) != 1 || hooks[0]["config"].(map[string]any)["url"] != whURL || len(hooks[0]["events"].([]any)) != len(gitea.LingoHookEvents) {
		t.Errorf("hooks = %+v", hooks)
	}
	if len(labels) != 8 || labels[0] != "lingo" {
		t.Errorf("labels = %v", labels)
	}
	mu.Unlock()
	// 再跑一次：Webhook 已存在，不重复创建
	rs = l.SelfCheck(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(hooks) != 1 {
		t.Errorf("second run created another hook: %+v", rs)
	}
}
