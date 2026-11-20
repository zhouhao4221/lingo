package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestOpenAIChatAndEmbed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer sk-x" || r.Header.Get("X-Org") != "o" {
			http.Error(w, `{"error":{"message":"bad auth"}}`, 401)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch r.URL.Path {
		case "/v1/chat/completions":
			if req["model"] != "gpt-test" || len(req["messages"].([]any)) != 2 || req["stream"] != false {
				http.Error(w, "bad request", 400)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}]}`))
		case "/v1/embeddings":
			_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0.2]},{"index":0,"embedding":[0.1]}]}`))
		}
	}))
	defer srv.Close()
	c, err := NewOpenAI(OpenAIOptions{BaseURL: srv.URL + "/v1/", APIKey: "sk-x", Model: "gpt-test", EmbeddingModel: "emb",
		Headers: map[string]string{"X-Org": "o"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Chat(context.Background(), []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}})
	if err != nil || out != "你好" {
		t.Errorf("chat = %q %v", out, err)
	}
	vs, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil || len(vs) != 2 || vs[0][0] != 0.1 || vs[1][0] != 0.2 {
		t.Errorf("embed = %v %v", vs, err)
	}
	c2, _ := NewOpenAI(OpenAIOptions{BaseURL: srv.URL + "/v1", Model: "m"})
	if _, err := c2.Embed(context.Background(), []string{"a"}); !errors.Is(err, ErrNoEmbedding) {
		t.Errorf("no embedding model: %v", err)
	}
	var ae *APIError
	if _, err := c2.Chat(context.Background(), nil); !errors.As(err, &ae) || ae.StatusCode != 401 || ae.Message != "bad auth" {
		t.Errorf("auth error = %v", err)
	}
}

func TestOpenAIResponseChecks(t *testing.T) {
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.Header().Set("Retry-After", "120")
			http.Error(w, strings.Repeat("服务繁忙", 200), 429)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, _ := NewOpenAI(OpenAIOptions{BaseURL: srv.URL, Model: "m", EmbeddingModel: "e"})
	for b, want := range map[string]error{
		`{"choices":[{"message":{"content":"半截"},"finish_reason":"length"}]}`:     ErrTruncated,
		`{"choices":[{"message":{"content":null},"finish_reason":"tool_calls"}]}`: ErrEmptyContent,
	} {
		body = b
		if _, err := c.Chat(context.Background(), nil); !errors.Is(err, want) {
			t.Errorf("%s: %v", b, err)
		}
	}
	body = `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`
	if _, err := c.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Error("duplicate embedding index accepted")
	}
	// Retry-After 超过上限：不重试；非 JSON 错误按字符截断，不产生无效 UTF-8
	cf, _ := NewOpenAI(OpenAIOptions{BaseURL: srv.URL + "/fail", Model: "m"})
	cf.o.BaseURL = srv.URL
	var ae *APIError
	start := time.Now()
	err := cf.do(context.Background(), "/fail", []byte("{}"), &struct{}{})
	if !errors.As(err, &ae) || !utf8.ValidString(ae.Message) {
		t.Errorf("err = %v", err)
	}
	if err := cf.post(context.Background(), "/fail", struct{}{}, &struct{}{}); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("long Retry-After retried: %v", err)
	}
}

func TestOpenAIRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, `{"error":{"message":"busy"}}`, 503)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c, _ := NewOpenAI(OpenAIOptions{BaseURL: srv.URL, Model: "m"})
	if out, err := c.Chat(context.Background(), nil); err != nil || out != "ok" || calls.Load() != 2 {
		t.Errorf("retry = %q %v calls=%d", out, err, calls.Load())
	}
	calls.Store(0)
	c0, _ := NewOpenAI(OpenAIOptions{BaseURL: srv.URL, Model: "m", MaxRetries: -1})
	if _, err := c0.Chat(context.Background(), nil); err == nil || calls.Load() != 1 {
		t.Errorf("no-retry = %v calls=%d", err, calls.Load())
	}
	for _, o := range []OpenAIOptions{{Model: "m"}, {BaseURL: "ftp://x", Model: "m"}, {BaseURL: "https://x"}} {
		if _, err := NewOpenAI(o); err == nil {
			t.Errorf("accepted %+v", o)
		}
	}
}
