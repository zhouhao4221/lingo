package tests

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo"
)

type mcpClient struct {
	t *testing.T
	h http.Handler
}

func (c mcpClient) call(user, role, tool string, args any) (map[string]any, bool) {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User", user)
	r.Header.Set("X-Role", role)
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	var resp struct {
		Result struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
			Content           []struct{ Text string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		c.t.Fatalf("%s: %d %s", tool, w.Code, w.Body)
	}
	if resp.Result.IsError {
		return map[string]any{"error": resp.Result.Content[0].Text}, false
	}
	return resp.Result.StructuredContent, true
}

func TestMCPAgentRepliesToSubmitter(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	db := testDB(t, true)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	rec := &recorder{}
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{}, Notifier: rec,
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		Projects: []lingo.Project{{Key: "demo", Name: "Demo", AutoForward: map[lingo.TicketType]bool{lingo.TicketBug: true},
			Repos: []lingo.Repo{{Owner: "demo", Name: "api", Kind: lingo.RepoBackend}}}},
		Authenticate: func(r *http.Request) (lingo.User, error) {
			if r.Header.Get("X-User") == "" {
				return lingo.User{}, errors.New("anonymous")
			}
			return lingo.User{ID: r.Header.Get("X-User"), Name: r.Header.Get("X-User"), Role: lingo.Role(r.Header.Get("X-Role"))}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := mcpClient{t: t, h: l.MCPHandler()}

	ps, ok := c.call("u1", "external", "list_projects", nil)
	if !ok || len(ps["items"].([]any)) != 1 || ps["items"].([]any)[0].(map[string]any)["repos"].([]any)[0].(map[string]any)["kind"] != "backend" {
		t.Errorf("list_projects = %v", ps)
	}
	created, ok := c.call("u1", "external", "create_ticket", map[string]any{"type": "bug", "content": "导出报错"})
	if !ok || created["status"] != "submitted" {
		t.Fatalf("create_ticket = %v", created)
	}
	id := int64(created["id"].(float64))
	if out, ok := c.call("u1", "external", "create_ticket", map[string]any{"type": "question", "content": "x"}); ok || !strings.Contains(out["error"].(string), "invalid input") {
		t.Errorf("bad type = %v", out)
	}
	if out, ok := c.call("u1", "external", "get_ticket", map[string]any{}); ok || !strings.Contains(out["error"].(string), "id is required") {
		t.Errorf("missing id = %v", out)
	}
	runLingo(t, l)
	waitTicket(t, l, lingo.User{ID: "u1", Role: lingo.RoleExternal}, id, "open")

	// 研发 Agent 查看并回复；他人看不到
	if out, ok := c.call("u2", "external", "get_ticket", map[string]any{"id": id}); ok || out["error"] != "lingo: not found" {
		t.Errorf("other user get = %v", out)
	}
	tk, ok := c.call("dev-agent", "agent", "get_ticket", map[string]any{"id": id})
	if !ok || tk["title"] != "导出报错" {
		t.Fatalf("agent get = %v", tk)
	}
	if out, ok := c.call("dev-agent", "agent", "reply_ticket", map[string]any{"id": id, "body": "已定位，下个版本修复"}); !ok || out["source"] != "agent" {
		t.Fatalf("reply = %v", out)
	}
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.comments) == 1
	})
	g.mu.Lock()
	posted := g.comments[0]
	g.mu.Unlock()
	if !strings.Contains(posted, "已定位，下个版本修复") || !strings.Contains(posted, "<!-- lingo:public -->") {
		t.Errorf("gitea comment = %q", posted)
	}
	mine, _ := c.call("u1", "external", "get_ticket", map[string]any{"id": id})
	cs := mine["comments"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["body"] != "已定位，下个版本修复" {
		t.Errorf("submitter comments = %v", cs)
	}
	evs := rec.waitFor(t, 3)
	if evs[2].Kind != lingo.EventTicketComment || evs[2].ActorID != "dev-agent" {
		t.Errorf("events = %+v", evs)
	}
	// 后台工具对 external 不可用
	if out, ok := c.call("u1", "external", "list_triage", nil); ok || !strings.Contains(out["error"].(string), "forbidden") {
		t.Errorf("external list_triage = %v", out)
	}
	if out, ok := c.call("r1", "reviewer", "list_releases", map[string]any{"project": "demo"}); !ok || len(out["items"].([]any)) != 0 {
		t.Errorf("list_releases = %v", out)
	}
}
