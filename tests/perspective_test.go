package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo"
)

// 需求多视角补充（REQ-001）：转出前补充随建单渲染进 Issue 区块并打视角标签，缺必填视角发 perspective_missing；
// 转出后补充经 outbox 重写区块、补标签、发提示评论。
func TestPerspectivesFlow(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	db := testDB(t, true)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{},
		Gitea:                lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		RequiredPerspectives: []lingo.Perspective{lingo.PerspectiveBusiness, lingo.PerspectiveLeader},
		Projects:             []lingo.Project{{Key: "demo", Name: "Demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate:         func(*http.Request) (lingo.User, error) { return lingo.User{}, nil },
		// 接入方规则：有视角的人可参与项目内全部需求，查看对所有人开放
		AuthorizePerspective: func(_ context.Context, u lingo.User, tk lingo.Ticket, write bool) error {
			if write && u.Perspective == "" && u.ID != tk.UserID {
				return lingo.ErrForbidden
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	biz := lingo.User{ID: "b1", Name: "张三", Email: "zhang@example.com", Role: lingo.RoleExternal, Perspective: lingo.PerspectiveBusiness}
	expert := lingo.User{ID: "e1", Name: "李四", Role: lingo.RoleExternal, Perspective: lingo.PerspectiveExpert}

	tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketFeature, Content: "希望支持导出 Excel"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.AddPerspective(ctx, lingo.User{ID: "x", Role: lingo.RoleExternal, Perspective: "boss"}, tk.ID, "a"); !errors.Is(err, lingo.ErrInvalidInput) {
		t.Errorf("bad perspective: %v", err)
	}
	if _, err := l.AddPerspective(ctx, lingo.User{ID: "u9", Role: lingo.RoleExternal}, tk.ID, "x"); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("hook denial: %v", err)
	}
	if _, err := l.AddPerspective(ctx, biz, tk.ID, "影响华东 30 家门店"); err != nil {
		t.Fatal(err)
	}
	ps, err := l.ListPerspectives(ctx, expert, tk.ID) // 他人的需求也可查看（权限由接入方判断）
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Groups) != 2 || !ps.Groups[0].Entries[0].Original || ps.Groups[0].Entries[0].Content != "希望支持导出 Excel" ||
		ps.Groups[1].Entries[0].AuthorName != "张三" || len(ps.Missing) != 1 || ps.Missing[0] != lingo.PerspectiveLeader {
		t.Fatalf("perspectives = %+v", ps)
	}

	if _, err := l.ForwardTicket(ctx, rita, tk.ID, lingo.ForwardOptions{}); err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	waitTicket(t, l, alice, tk.ID, "open")
	g.mu.Lock()
	body, _ := g.created[0]["body"].(string)
	nLabels := len(g.created[0]["labels"].([]any))
	g.mu.Unlock()
	for _, want := range []string{"<!-- lingo:perspectives -->", "#### 使用者反馈\n\n**Alice · ", "原始提交见正文", "#### 业务补充\n\n**张三 · ",
		"> 影响华东 30 家门店", "> 缺少：决策意见", "<!-- /lingo:perspectives -->"} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "zhang@example.com") || strings.Contains(body, "alice@example.com") || strings.Count(body, "希望支持导出 Excel") != 1 {
		t.Errorf("issue body leaks email or repeats content:\n%s", body)
	}
	if nLabels != 4 { // 来源、类型、视角/使用者、视角/业务方
		t.Errorf("labels = %d", nLabels)
	}
	// 变更流只返回早于所有进行中事务的事件（并发测试共用数据库时可能稍有延迟）：等到两类事件都出现
	var evs []lingo.Event
	waitFor(t, func() bool {
		evs, err = l.ListEvents(ctx, rita, 0, 100)
		n := 0
		for _, e := range evs {
			if e.Kind == lingo.EventTicketPerspectiveAdded || e.Kind == lingo.EventTicketPerspectiveMissing {
				n++
			}
		}
		return err == nil && n >= 2
	})
	var added, missing int
	for _, e := range evs {
		switch e.Kind {
		case lingo.EventTicketPerspectiveAdded:
			added++
			if e.Body != "业务方 · 张三 补充了内容" || e.Perspectives[0] != lingo.PerspectiveBusiness {
				t.Errorf("added event = %+v", e)
			}
		case lingo.EventTicketPerspectiveMissing:
			missing++
			if e.Body != "缺少：决策意见" || len(e.Perspectives) != 1 || e.Perspectives[0] != lingo.PerspectiveLeader {
				t.Errorf("missing event = %+v", e)
			}
		}
	}
	if added != 1 || missing != 1 {
		t.Errorf("added %d missing %d in %+v", added, missing, evs)
	}

	// 转出后补充：重写区块（区块外不变）、补打视角标签、发提示评论
	if _, err := l.AddPerspective(ctx, expert, tk.ID, "建议异步生成，避免超时"); err != nil {
		t.Fatal(err)
	}
	lead := lingo.User{ID: "l1", Role: lingo.RoleExternal, Perspective: lingo.PerspectiveLeader}
	if _, err := l.AddPerspective(ctx, lead, tk.ID, "本月上线，P1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.comments) == 2 && len(g.edits) > 0 && strings.Contains(g.edits[len(g.edits)-1], "本月上线")
	})
	g.mu.Lock()
	last := g.edits[len(g.edits)-1]
	comments := append([]string(nil), g.comments...)
	added2 := len(g.added)
	g.mu.Unlock()
	if !strings.Contains(last, "#### 专家意见\n\n**李四 · ") || !strings.Contains(last, "#### 决策意见") || strings.Contains(last, "缺少") ||
		strings.Count(last, "<!-- lingo:perspectives -->") != 1 || !strings.HasPrefix(last, body[:strings.Index(body, "<!-- lingo:perspectives -->")]) {
		t.Errorf("rewritten body:\n%s", last)
	}
	if comments[0] != "**专家 · 李四 补充了内容**，见正文「需求视角补充」。" || added2 == 0 {
		t.Errorf("comments = %v, added labels = %d", comments, added2)
	}
	var posted int
	if err := db.QueryRow("SELECT count(*) FROM lingo_ticket_perspectives WHERE gitea_posted_id IS NOT NULL").Scan(&posted); err != nil || posted != 2 {
		t.Errorf("posted = %d, %v", posted, err)
	}
}

func TestPerspectivesREST(t *testing.T) {
	_, c := newRESTLingo(t)
	code, tk, _ := c.do("u1", "external", "POST", "/tickets", map[string]any{"type": "feature", "content": "导出"})
	if code != 201 {
		t.Fatalf("create = %d %v", code, tk)
	}
	id := jsonNum(int64(tk["id"].(float64)))
	// 未配置 AuthorizePerspective：按工单可见范围（他人与 agent 看不到待确认的需求）
	if code, _, _ := c.do("u2", "external", "POST", "/tickets/"+id+"/perspectives", map[string]any{"content": "我也需要"}); code != 404 {
		t.Errorf("other external add = %d", code)
	}
	if code, _, _ := c.do("a1", "agent", "GET", "/tickets/"+id+"/perspectives", nil); code != 404 {
		t.Errorf("agent list pending = %d", code)
	}
	if code, p, _ := c.do("u1", "external", "POST", "/tickets/"+id+"/perspectives", map[string]any{"content": "补充一下"}); code != 201 || p["perspective"] != "user" {
		t.Errorf("owner add = %d %v", code, p)
	}
	code, list, _ := c.do("r1", "reviewer", "GET", "/tickets/"+id+"/perspectives", nil)
	if code != 200 || len(list["groups"].([]any)) != 1 || len(list["missing"].([]any)) != 0 {
		t.Errorf("list = %d %v", code, list)
	}
	for _, bad := range []string{"user_id", "email", "comments", "attachments"} {
		if strings.Contains(jsonString(list), "\""+bad+"\"") {
			t.Errorf("list leaks %s: %v", bad, list)
		}
	}
	if code, _, _ := c.do("u1", "external", "POST", "/tickets/"+id+"/perspectives", map[string]any{"content": " "}); code != 400 {
		t.Errorf("empty content = %d", code)
	}
	_, bug, _ := c.do("u1", "external", "POST", "/tickets", map[string]any{"type": "bug", "content": "白屏"})
	if code, _, _ := c.do("u1", "external", "POST", "/tickets/"+jsonNum(int64(bug["id"].(float64)))+"/perspectives", map[string]any{"content": "x"}); code != 400 {
		t.Errorf("bug ticket = %d", code)
	}
	if code, _, _ := c.do("u1", "external", "GET", "/tickets/99999/perspectives", nil); code != 404 {
		t.Errorf("missing ticket = %d", code)
	}
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
