package tests

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zhouhao4221/lingo"
)

// draftModel 是轻量模型的替身：记录收到的提示，返回预设输出。
type draftModel struct {
	mu     sync.Mutex
	out    string
	err    error
	prompt string
}

func (m *draftModel) Chat(_ context.Context, ms []lingo.Message) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prompt = ms[len(ms)-1].Content
	return m.out, m.err
}
func (m *draftModel) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

func newDraftLingo(t *testing.T, g *giteaServer, m lingo.Model) (*lingo.Lingo, *sql.DB) {
	t.Helper()
	db := testDB(t, true)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, LightModel: m, SkipSelfCheck: true, ReconcileInterval: -1,
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		Projects: []lingo.Project{{Key: "demo", AutoForward: map[lingo.TicketType]bool{lingo.TicketBug: true},
			Repos: []lingo.Repo{{Owner: "demo", Name: "api", Templates: map[string]string{"bug": "bug_report.yaml"}}}}},
		Authenticate: func(*http.Request) (lingo.User, error) { return lingo.User{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return l, db
}

const draftTemplate = `[{"name":"Bug","title":"[Bug]: ","labels":["type/bug"],"file_name":"bug_report.yaml","body":[
  {"type":"textarea","id":"what","attributes":{"label":"发生了什么"},"validations":{"required":true}},
  {"type":"textarea","id":"steps","attributes":{"label":"复现步骤"}},
  {"type":"dropdown","id":"browser","attributes":{"label":"浏览器","options":["Chrome","Safari"]}}]}]`

func TestLightModelDraftsIssue(t *testing.T) {
	g := &giteaServer{templates: draftTemplate}
	m := &draftModel{out: `{"title":"Safari 下登录后页面白屏","description":"- 登录成功后页面空白\n- 刷新无效",
		"fields":{"what":["登录后白屏"],"steps":["1. 打开登录页\n2. 输入账号密码登录"],"browser":["Safari"]}}`}
	l, _ := newDraftLingo(t, g, m)
	ctx := context.Background()
	tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketBug,
		Content: "我用 safari 打开登录页，输了账号密码登录，然后页面就白了，刷新也没用。急！！"})
	if err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	waitTicket(t, l, alice, tk.ID, "open")
	g.mu.Lock()
	req := g.created[0]
	g.mu.Unlock()
	body, _ := req["body"].(string)
	if req["title"] != "[Bug]: Safari 下登录后页面白屏" {
		t.Errorf("title = %v", req["title"])
	}
	for _, want := range []string{"### 发生了什么\n\n登录后白屏", "### 复现步骤\n\n1. 打开登录页", "### 浏览器\n\nSafari",
		"### 补充说明\n\n- 登录成功后页面空白", "<summary>提交人原始描述</summary>", "急！！", "<!-- lingo:ticket="} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	m.mu.Lock()
	prompt := m.prompt
	m.mu.Unlock()
	if strings.Contains(prompt, "Alice") || strings.Contains(prompt, "alice@example.com") || !strings.Contains(prompt, "browser") {
		t.Errorf("prompt leaks identity or misses template:\n%s", prompt)
	}
	// 工单本身的标题与正文不变（提交人看到的是自己写的）
	if got, _ := l.GetTicket(ctx, alice, tk.ID); got.Body != tk.Body || got.Title != tk.Title {
		t.Errorf("ticket changed: %+v", got)
	}
}

func TestLightModelKeepsSubmitterTitle(t *testing.T) {
	g := &giteaServer{templates: draftTemplate}
	l, _ := newDraftLingo(t, g, &draftModel{out: `{"title":"模型改写的标题","description":"整理后的描述"}`})
	tk, err := l.CreateTicket(context.Background(), alice, lingo.TicketInput{Type: lingo.TicketBug, Title: "Export CSV 500 on /reports", Content: "导出报 500"})
	if err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	waitTicket(t, l, alice, tk.ID, "open")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.created[0]["title"] != "[Bug]: Export CSV 500 on /reports" {
		t.Errorf("title = %v", g.created[0]["title"])
	}
}

func TestLightModelFailureFallsBack(t *testing.T) {
	g := &giteaServer{templates: draftTemplate}
	l, _ := newDraftLingo(t, g, &draftModel{err: errors.New("model unavailable")})
	tk, err := l.CreateTicket(context.Background(), alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "导出报错"})
	if err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	waitTicket(t, l, alice, tk.ID, "open")
	g.mu.Lock()
	defer g.mu.Unlock()
	body, _ := g.created[0]["body"].(string)
	if g.created[0]["title"] != "[Bug]: 导出报错" || strings.Contains(body, "原始描述") || !strings.Contains(body, "导出报错") {
		t.Errorf("fallback issue = %v\n%s", g.created[0]["title"], body)
	}
}
