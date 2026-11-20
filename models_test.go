package lingo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewOpenAIModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	m, err := NewOpenAIModel(OpenAIConfig{BaseURL: srv.URL + "/v1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := m.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil || out != "ok" {
		t.Errorf("chat = %q %v", out, err)
	}
	if _, err := m.Embed(context.Background(), []string{"x"}); err == nil {
		t.Error("embed without embedding model succeeded")
	}
	if _, err := NewOpenAIModel(OpenAIConfig{}); err == nil {
		t.Error("missing model accepted")
	}
	if _, err := NewDeepSeekModel("sk", ""); err != nil {
		t.Errorf("deepseek: %v", err)
	}
}
