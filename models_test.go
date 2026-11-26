package lingo

import (
	"context"
	"errors"
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

func TestModelImageCapability(t *testing.T) {
	text, err := NewOpenAIModel(OpenAIConfig{BaseURL: "http://127.0.0.1:1/v1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	vision, err := NewOpenAIModel(OpenAIConfig{BaseURL: "http://127.0.0.1:1/v1", Model: "m", Images: true})
	if err != nil {
		t.Fatal(err)
	}
	ds, err := NewDeepSeekModel("k", "")
	if err != nil {
		t.Fatal(err)
	}
	if supportsImages(text) || !supportsImages(vision) || supportsImages(ds) || supportsImages(nil) {
		t.Errorf("capability: text=%v vision=%v deepseek=%v", supportsImages(text), supportsImages(vision), supportsImages(ds))
	}
	if _, ok := vision.(ImageModel); !ok {
		t.Error("vision model must implement ImageModel")
	}
}

func TestQuestionAttachments(t *testing.T) {
	if _, err := questionAttachments([]Attachment{{ID: "a", ContentType: "image/png", URL: "https://files.example.com/a.png"}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("url accepted: %v", err)
	}
	got, err := questionAttachments([]Attachment{{ID: "a", Name: "x.png", ContentType: "image/png", Size: 3, Forwarded: true, GiteaURL: "g", CommentID: 9}})
	if err != nil || len(got) != 1 || got[0].ID != "a" || got[0].Name != "x.png" || got[0].Size != 3 {
		t.Errorf("got %+v %v", got, err)
	}
}
