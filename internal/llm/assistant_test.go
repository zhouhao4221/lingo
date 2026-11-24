package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/assistant"
)

var testProjects = []assistant.ProjectInfo{{Key: "demo", Repos: []string{"demo/api", "demo/web"}}, {Key: "ops", Repos: []string{"ops/infra"}}}

func TestParseIntentValidates(t *testing.T) {
	in := parseIntent("好的，结果如下：\n```json\n"+`{"intent":"ticket_summary","ticket_ids":[3,3,-1],"project":"demo","repo":"DEMO/WEB",
		"type":"bug","statuses":["pending_triage","bogus"],"keywords":["登录","","登录","a","b","c","d"],
		"time":{"field":"forwarded","preset":"this_week","from":"2026-01-01"},"release":{"repo":"ops/infra","tag":" v1.2.0 "},
		"issue_number":5,"title":"  本周   转出 "}`+"\n```", testProjects)
	switch {
	case in.Kind != assistant.IntentTicketSummary, len(in.TicketIDs) != 1 || in.TicketIDs[0] != 3, in.Project != "demo",
		in.Repo != "demo/web", in.Type != "bug", len(in.Statuses) != 1, len(in.Keywords) != 5,
		in.Time.Field != "forwarded" || in.Time.Preset != "this_week" || in.Time.From != "",
		in.Release.Repo != "" /* ops/infra 不属于 demo */, in.Release.Tag != "v1.2.0", in.IssueExplicit, in.Title != "本周 转出":
		t.Errorf("intent = %+v", in)
	}
	if in := parseIntent(`{"intent":"ticket_find","project":"secret"}`, testProjects); !in.UnknownProject || in.Project != "" {
		t.Errorf("unknown project: %+v", in)
	}
	if in := parseIntent(`{"intent":"ticket_find","issue_number":7,"issue_explicit":true,"time":{"from":"bad","to":"2026-02-01"}}`, testProjects); !in.IssueExplicit || in.IssueNumber != 7 || in.Time.From != "" || in.Time.To != "2026-02-01" {
		t.Errorf("issue / dates: %+v", in)
	}
	for _, out := range []string{"不是 JSON", `{"intent":"drop_tables"}`, `{}`} {
		if in := parseIntent(out, testProjects); in.Kind != assistant.IntentClarify {
			t.Errorf("%q: %+v", out, in)
		}
	}
}

func TestAssistantConfirmAndAnswer(t *testing.T) {
	var got []Message
	chat := func(out string) Chat {
		return func(_ context.Context, ms []Message) (string, Usage, error) { got = ms; return out, Usage{}, nil }
	}
	var kinds []string
	a := &Assistant{Light: chat(`{"decision":"confirm"}`), Main: chat(" 回答 "),
		Record: func(_ context.Context, kind string, _ Usage, _ bool, _ time.Duration) { kinds = append(kinds, kind) }}
	ctx := context.Background()
	if d, err := a.Confirm(ctx, assistant.ConfirmInput{Summary: "在 #1 留言：x", Message: "好的"}); err != nil || d != "confirm" {
		t.Errorf("confirm = %q %v", d, err)
	}
	a.Light = chat(`{"decision":"delete everything"}`)
	if d, _ := a.Confirm(ctx, assistant.ConfirmInput{}); d != assistant.IntentOtherDecision {
		t.Errorf("bad decision = %q", d)
	}
	var deltas []string
	out, err := a.Answer(ctx, assistant.AnswerInput{History: []assistant.HistoryItem{{Role: "user", Content: "早"}, {Role: "assistant", Content: "你好"}},
		Question: "问", Facts: []byte(`{"kind":"x"}`)}, func(d string) { deltas = append(deltas, d) })
	if err != nil || out != "回答" || len(deltas) != 1 || deltas[0] != "回答" {
		t.Errorf("answer = %q %v %v", out, err, deltas)
	}
	if len(got) != 4 || got[0].Role != "system" || !strings.Contains(got[3].Content, `{"kind":"x"}`) {
		t.Errorf("answer messages = %+v", got)
	}
	if len(kinds) != 3 || kinds[2] != KindAssistantAnswer {
		t.Errorf("usage kinds = %v", kinds)
	}
}

func TestDecodeFirstResetsBetweenCandidates(t *testing.T) {
	in := parseIntent(`示例 {"mine":true,"ticket_ids":[1],"intent":5} 实际 {"intent":"ticket_find"}`, testProjects)
	if in.Kind != assistant.IntentTicketFind || in.Mine || len(in.TicketIDs) != 0 {
		t.Errorf("leaked fields: %+v", in)
	}
}

func TestOpenAIChatStreamIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"半截"}}]}` + "\n\n")) // 没有 finish_reason 与 [DONE] 就断开
	}))
	defer srv.Close()
	c, _ := NewOpenAI(OpenAIOptions{BaseURL: srv.URL, Model: "m"})
	if _, _, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, func(string) {}); !errors.Is(err, ErrStreamIncomplete) {
		t.Errorf("err = %v, want ErrStreamIncomplete", err)
	}
}

func TestOpenAIChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range []string{
			`data: {"model":"m-1","choices":[{"delta":{"content":"你"}}]}`,
			`data: {"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`,
			"data: [DONE]",
		} {
			_, _ = w.Write([]byte(l + "\n\n"))
		}
	}))
	defer srv.Close()
	c, err := NewOpenAI(OpenAIOptions{BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	out, u, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, func(d string) { parts = append(parts, d) })
	if err != nil || out != "你好" || len(parts) != 2 || u.Model != "m-1" || u.Input != 9 || u.Cached != 4 || u.Output != 2 {
		t.Errorf("stream = %q %v %+v %v", out, parts, u, err)
	}
}
