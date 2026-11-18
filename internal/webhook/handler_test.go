package webhook

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zhouhao4221/lingo/internal/gitea"
)

// memStore 按 delivery ID 与业务键去重，模拟 webhook_deliveries 的两个唯一约束。
type memStore struct {
	mu      sync.Mutex
	byID    map[string]bool
	byKey   map[string]bool
	records []Record
	err     error
}

func newMemStore() *memStore { return &memStore{byID: map[string]bool{}, byKey: map[string]bool{}} }

func (s *memStore) SaveDelivery(_ context.Context, r Record) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false, s.err
	}
	if s.byID[r.DeliveryID] || s.byKey[r.DedupKey] {
		return false, nil
	}
	s.byID[r.DeliveryID], s.byKey[r.DedupKey] = true, true
	s.records = append(s.records, r)
	return true, nil
}

const secret = "s3cret"

func newTestHandler(t *testing.T, st Store, mod func(*Options)) *Handler {
	t.Helper()
	o := Options{Secret: secret, Store: st, BotLogin: func() string { return "lingo-bot" },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mod != nil {
		mod(&o)
	}
	h, err := NewHandler(o)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func post(h http.Handler, deliveryID, eventType, body string, sign bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/webhooks/gitea", strings.NewReader(body))
	r.Header.Set(gitea.HeaderDelivery, deliveryID)
	r.Header.Set(gitea.HeaderEvent, eventType)
	r.Header.Set(gitea.HeaderEventType, eventType)
	if sign {
		r.Header.Set(gitea.HeaderSignature, hex.EncodeToString(gitea.Sign(secret, []byte(body))))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNewHandlerValidates(t *testing.T) {
	if _, err := NewHandler(Options{Store: newMemStore()}); err == nil {
		t.Error("missing secret accepted")
	}
	if _, err := NewHandler(Options{Secret: "x"}); err == nil {
		t.Error("missing store accepted")
	}
}

func TestHandlerStoresAndDedups(t *testing.T) {
	st := newMemStore()
	h := newTestHandler(t, st, nil)
	body := commentBody("created", "/reply 已修复", "")

	if w := post(h, "d-1", gitea.EventIssueComment, body, true); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	// 同一 delivery 的重复请求
	if w := post(h, "d-1", gitea.EventIssueComment, body, true); w.Code != 200 || w.Body.String() != "duplicate" {
		t.Errorf("same delivery: %d %s", w.Code, w.Body)
	}
	// Gitea 手动重投：新的 delivery ID、相同载荷，由业务键挡住
	if w := post(h, "d-2", gitea.EventIssueComment, body, true); w.Body.String() != "duplicate" {
		t.Errorf("replay: %d %s", w.Code, w.Body)
	}
	if len(st.records) != 1 {
		t.Fatalf("records = %d", len(st.records))
	}
	r := st.records[0]
	if r.Kind != CommentCreated || r.Repo != "demo/api" || r.IssueNumber != 12 || r.EventType != gitea.EventIssueComment ||
		string(r.Payload) != body || r.FromBot {
		t.Errorf("record = %+v", r)
	}
}

func TestHandlerMarksBotEvents(t *testing.T) {
	st := newMemStore()
	h := newTestHandler(t, st, func(o *Options) { o.BotLogin = func() string { return "dev-agent" } })
	post(h, "d-1", gitea.EventIssueComment, commentBody("created", "x", ""), true)
	if len(st.records) != 1 || !st.records[0].FromBot {
		t.Errorf("records = %+v", st.records)
	}
}

func TestHandlerRejects(t *testing.T) {
	st := newMemStore()
	h := newTestHandler(t, st, func(o *Options) { o.MaxBytes = 2048 })
	body := issueBody("opened", "", "")

	if w := post(h, "d-1", gitea.EventIssues, body, false); w.Code != 401 {
		t.Errorf("unsigned: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set(gitea.HeaderSignature, "00")
	r.Header.Set(gitea.HeaderDelivery, "d-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("bad signature: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Errorf("GET: %d", w.Code)
	}
	if w := post(h, "d-1", gitea.EventIssues, body+strings.Repeat(" ", 4096), true); w.Code != 413 {
		t.Errorf("too large: %d", w.Code)
	}
	if w := post(h, "", gitea.EventIssues, body, true); w.Code != 400 {
		t.Errorf("missing delivery id: %d", w.Code)
	}
	if w := post(h, "d-1", gitea.EventIssues, `{"action":"opened"}`, true); w.Code != 400 {
		t.Errorf("unparsable: %d", w.Code)
	}
	if len(st.records) != 0 {
		t.Errorf("rejected requests stored: %+v", st.records)
	}
}

func TestHandlerAuthorizationHeader(t *testing.T) {
	h := newTestHandler(t, newMemStore(), func(o *Options) { o.Authorization = "Bearer abc" })
	body := issueBody("opened", "", "")
	if w := post(h, "d-1", gitea.EventIssues, body, true); w.Code != 401 {
		t.Errorf("missing authorization: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer abc")
	r.Header.Set(gitea.HeaderDelivery, "d-1")
	r.Header.Set(gitea.HeaderEventType, gitea.EventIssues)
	r.Header.Set(gitea.HeaderSignature, hex.EncodeToString(gitea.Sign(secret, []byte(body))))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("with authorization: %d %s", w.Code, w.Body)
	}
}

func TestHandlerIgnoredNotStored(t *testing.T) {
	st := newMemStore()
	h := newTestHandler(t, st, nil)
	if w := post(h, "d-1", "wiki", `{}`, true); w.Code != 200 || w.Body.String() != "ignored" {
		t.Errorf("ignored: %d %s", w.Code, w.Body)
	}
	if len(st.records) != 0 {
		t.Errorf("ignored event stored")
	}
}

func TestHandlerStoresPing(t *testing.T) {
	st := newMemStore()
	h := newTestHandler(t, st, nil)
	post(h, "d-9", gitea.EventPush, `{"ref":"refs/heads/main",`+repoJSON+`}`, true)
	if len(st.records) != 1 || st.records[0].Kind != Ping || st.records[0].Repo != "demo/api" {
		t.Errorf("ping records = %+v", st.records)
	}
}

func TestHandlerStoreError(t *testing.T) {
	st := newMemStore()
	st.err = errors.New("db down")
	h := newTestHandler(t, st, nil)
	if w := post(h, "d-1", gitea.EventIssues, issueBody("opened", "", ""), true); w.Code != 500 {
		t.Errorf("store error: %d", w.Code)
	}
}
