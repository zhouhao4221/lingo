package tests

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo"
	"github.com/zhouhao4221/lingo/internal/gitea"
)

type nopModel struct{}

func (nopModel) Chat(context.Context, []lingo.Message) (string, error) { return "", nil }
func (nopModel) Embed(context.Context, []string) ([][]float32, error)  { return nil, nil }

func newLingo(t *testing.T, secret string) (*lingo.Lingo, func(query string) int) {
	t.Helper()
	db := testDB(t, true)
	// 模拟 Gitea：只需响应 GET /user（机器人账号）
	gsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"login":"lingo-bot"}`))
	}))
	t.Cleanup(gsrv.Close)
	l, err := lingo.New(lingo.Config{
		DB:           db,
		Model:        nopModel{},
		Gitea:        lingo.GiteaConfig{BaseURL: gsrv.URL, Token: "t", WebhookSecret: secret},
		Projects:     []lingo.Project{{Key: "demo", Name: "Demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate: func(*http.Request) (lingo.User, error) { return lingo.User{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	count := func(query string) int {
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return l, count
}

func deliver(h http.Handler, id, eventType, body, secret string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/webhooks/gitea", strings.NewReader(body))
	r.Header.Set(gitea.HeaderDelivery, id)
	r.Header.Set(gitea.HeaderEvent, eventType)
	r.Header.Set(gitea.HeaderEventType, eventType)
	r.Header.Set(gitea.HeaderSignature, hex.EncodeToString(gitea.Sign(secret, []byte(body))))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const commentPayload = `{"action":"created","issue":{"id":100,"number":12,"user":{"login":"lingo-bot"},"body":"x"},
  "comment":{"id":99,"body":"/reply 已修复","updated_at":"2026-11-18T01:00:00Z"},
  "repository":{"full_name":"demo/api"},"sender":{"login":"dev-agent"}}`

func TestWebhookHandlerStoresDeliveries(t *testing.T) {
	l, count := newLingo(t, "s3cret")
	h := l.WebhookHandler()
	if w := deliver(h, "d-1", "issue_comment", commentPayload, "s3cret"); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("deliver: %d %s", w.Code, w.Body)
	}
	if w := deliver(h, "d-2", "issue_comment", commentPayload, "s3cret"); w.Body.String() != "duplicate" {
		t.Errorf("replay: %d %s", w.Code, w.Body)
	}
	if w := deliver(h, "d-3", "issue_comment", commentPayload, "wrong"); w.Code != 401 {
		t.Errorf("bad signature: %d", w.Code)
	}
	if w := deliver(h, "d-4", "push", `{"repository":{"full_name":"demo/api"}}`, "s3cret"); w.Body.String() != "pong" {
		t.Errorf("ping: %d %s", w.Code, w.Body)
	}
	if n := count("SELECT count(*) FROM lingo_webhook_deliveries WHERE kind = 'comment.created' AND repo = 'demo/api' AND issue_number = 12"); n != 1 {
		t.Errorf("stored deliveries = %d", n)
	}
	if n := count("SELECT count(*) FROM lingo_repo_state WHERE repo = 'demo/api' AND kind = 'ping'"); n != 1 {
		t.Errorf("pings = %d", n)
	}
}

func TestWebhookHandlerWithoutSecretRejects(t *testing.T) {
	l, count := newLingo(t, "")
	if w := deliver(l.WebhookHandler(), "d-1", "issue_comment", commentPayload, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no secret: %d", w.Code)
	}
	if n := count("SELECT count(*) FROM lingo_webhook_deliveries"); n != 0 {
		t.Errorf("stored without secret: %d", n)
	}
}

func TestNewRejectsAutoRegisterWithoutSecret(t *testing.T) {
	db := testDB(t, false)
	_, err := lingo.New(lingo.Config{
		DB:           db,
		Model:        nopModel{},
		Gitea:        lingo.GiteaConfig{BaseURL: "http://g", Token: "t", AutoRegisterWebhook: true, WebhookURL: "http://app/wh"},
		Projects:     []lingo.Project{{Key: "demo"}},
		Authenticate: func(*http.Request) (lingo.User, error) { return lingo.User{}, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "WebhookSecret") {
		t.Errorf("err = %v", err)
	}
}
