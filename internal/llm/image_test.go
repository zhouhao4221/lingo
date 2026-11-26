package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zhouhao4221/lingo/internal/assistant"
)

// 合成数据：不是任何真实业务图片。
var testImage = assistant.Image{ContentType: "image/png", Data: []byte("\x89PNG synthetic")}

func TestOpenAIImagesWireFormat(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		bodies = append(bodies, req)
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"看到了\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"看到了"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c, err := NewOpenAI(OpenAIOptions{BaseURL: srv.URL, Model: "vision", Images: true})
	if err != nil || !c.SupportsImages() {
		t.Fatal(err)
	}
	ms := []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "这个报错怎么办", Images: []Image{testImage}}}
	if out, err := c.Chat(context.Background(), ms); err != nil || out != "看到了" {
		t.Fatalf("chat = %q %v", out, err)
	}
	if out, _, err := c.ChatStream(context.Background(), ms, nil); err != nil || out != "看到了" {
		t.Fatalf("stream = %q %v", out, err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(testImage.Data)
	for i, b := range bodies {
		msgs := b["messages"].([]any)
		if s, ok := msgs[0].(map[string]any)["content"].(string); !ok || s != "s" {
			t.Errorf("#%d: text-only message must stay a string: %v", i, msgs[0])
		}
		parts, ok := msgs[1].(map[string]any)["content"].([]any)
		if !ok || len(parts) != 2 {
			t.Fatalf("#%d: content = %v", i, msgs[1])
		}
		p0, p1 := parts[0].(map[string]any), parts[1].(map[string]any)
		if p0["type"] != "text" || p0["text"] != "这个报错怎么办" || p1["type"] != "image_url" ||
			p1["image_url"].(map[string]any)["url"] != want {
			t.Errorf("#%d: parts = %v", i, parts)
		}
	}
	// 只有图片没有文字：只发 image_url 片段
	bodies = nil
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Images: []Image{testImage}}}); err != nil {
		t.Fatal(err)
	}
	if parts := bodies[0]["messages"].([]any)[0].(map[string]any)["content"].([]any); len(parts) != 1 {
		t.Errorf("image-only parts = %v", parts)
	}
}

func TestOpenAIImagesNotDeclared(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c, _ := NewOpenAI(OpenAIOptions{BaseURL: srv.URL, Model: "text-only"})
	ms := []Message{{Role: "user", Content: "q", Images: []Image{testImage}}}
	if _, err := c.Chat(context.Background(), ms); !errors.Is(err, ErrImagesUnsupported) {
		t.Errorf("chat: %v", err)
	}
	if _, _, err := c.ChatStream(context.Background(), ms, nil); !errors.Is(err, ErrImagesUnsupported) {
		t.Errorf("stream: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("images must not be sent to a model that did not declare support (%d calls)", calls.Load())
	}
}

func TestAssistantImagesRouting(t *testing.T) {
	type call struct {
		model, sys string
		images     int
	}
	var calls []call
	chat := func(name, out string) Chat {
		return func(_ context.Context, ms []Message) (string, Usage, error) {
			calls = append(calls, call{name, ms[0].Content, len(ms[len(ms)-1].Images)})
			return out, Usage{}, nil
		}
	}
	intent := `{"intent":"knowledge_ask","keywords":["导出"]}`
	in := assistant.IntentInput{Today: "2026-11-26 星期四", Projects: testProjects, Question: "", Images: []assistant.Image{testImage}}

	// 轻量模型不支持图片：带图的意图解析改用主模型
	a := &Assistant{Light: chat("light", intent), Main: chat("main", intent), MainImages: true}
	if !a.SupportsImages() {
		t.Fatal("main declares images")
	}
	got, err := a.Intent(context.Background(), in)
	if err != nil || got.Kind != assistant.IntentKnowledgeAsk || len(calls) != 1 || calls[0].model != "main" || calls[0].images != 1 ||
		!strings.Contains(calls[0].sys, "图片") {
		t.Fatalf("intent with images: %+v %v %+v", got, err, calls)
	}
	// 轻量模型也支持：用轻量模型
	calls = nil
	a.LightImages = true
	if _, err := a.Intent(context.Background(), in); err != nil || calls[0].model != "light" {
		t.Errorf("light with images: %v %+v", err, calls)
	}
	// 不带图：提示词与之前完全相同，仍用轻量模型
	calls = nil
	in.Images, in.Question = nil, "导出为什么是空的"
	if _, err := a.Intent(context.Background(), in); err != nil || calls[0].model != "light" || calls[0].images != 0 ||
		calls[0].sys != intentSystemPrompt {
		t.Errorf("text-only intent changed: %+v", calls)
	}
	// 回答：图片随最后一条用户消息发给主模型，提示词追加图片规则
	calls = nil
	if _, err := a.Answer(context.Background(), assistant.AnswerInput{Question: "", Facts: json.RawMessage(`{"kind":"knowledge"}`),
		Images: []assistant.Image{testImage}}, nil); err != nil || calls[0].model != "main" || calls[0].images != 1 ||
		!strings.Contains(calls[0].sys, "用户随问题附了图片") {
		t.Errorf("answer with images: %v %+v", err, calls)
	}
	calls = nil
	if _, err := a.Answer(context.Background(), assistant.AnswerInput{Question: "q", Facts: json.RawMessage(`{"kind":"knowledge"}`)}, nil); err != nil ||
		calls[0].sys != answerSystemPrompt {
		t.Errorf("text-only answer prompt changed: %+v", calls)
	}
	// 都不支持：明确报错，不调用模型
	calls = nil
	b := &Assistant{Light: chat("light", intent), Main: chat("main", "x")}
	if _, err := b.Intent(context.Background(), assistant.IntentInput{Images: []assistant.Image{testImage}}); !errors.Is(err, ErrImagesUnsupported) {
		t.Errorf("intent unsupported: %v", err)
	}
	if _, err := b.Answer(context.Background(), assistant.AnswerInput{Images: []assistant.Image{testImage}}, nil); !errors.Is(err, ErrImagesUnsupported) {
		t.Errorf("answer unsupported: %v", err)
	}
	if b.SupportsImages() || len(calls) != 0 {
		t.Errorf("unsupported: %v %+v", b.SupportsImages(), calls)
	}
}
