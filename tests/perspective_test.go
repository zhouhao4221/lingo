package tests

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// newPerspectiveLingo 创建按请求头 X-User / X-Role / X-Perspective 认证、放行全部视角补充的实例。
func newPerspectiveLingo(t *testing.T, g *giteaServer) (*lingo.Lingo, *sql.DB) {
	t.Helper()
	db := testDB(t, true)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{},
		Gitea:                lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		RequiredPerspectives: []lingo.Perspective{lingo.PerspectiveLeader},
		Projects:             []lingo.Project{{Key: "demo", Name: "Demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate: func(r *http.Request) (lingo.User, error) {
			if r.Header.Get("X-User") == "" {
				return lingo.User{}, errors.New("anonymous")
			}
			return lingo.User{ID: r.Header.Get("X-User"), Name: r.Header.Get("X-User"), Role: lingo.Role(r.Header.Get("X-Role")),
				Perspective: lingo.Perspective(r.Header.Get("X-Perspective"))}, nil
		},
		AuthorizePerspective: func(context.Context, lingo.User, lingo.Ticket, bool) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return l, db
}

// 并发补充后 Issue 区块与库一致；改标题 / 备注转出区块照常生成；lingo 自己改正文触发的 edited Webhook 不引起视角变化；
// 视角事件对 external 只在本人工单可见。
func TestPerspectivesConcurrentWebhookAndEvents(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	l, db := newPerspectiveLingo(t, g)
	ctx := context.Background()
	tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketFeature, Content: "批量导出"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.ForwardTicket(ctx, rita, tk.ID, lingo.ForwardOptions{Title: "支持批量导出", Note: "优先处理"}); err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	waitTicket(t, l, alice, tk.ID, "open")
	g.mu.Lock()
	created, _ := g.created[0]["body"].(string)
	g.mu.Unlock()
	if !strings.Contains(created, "<!-- lingo:perspectives -->") || !strings.Contains(created, "> 优先处理") || !strings.Contains(created, "> 缺少：决策意见") {
		t.Fatalf("forwarded with title/note:\n%s", created)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := []lingo.Perspective{lingo.PerspectiveBusiness, lingo.PerspectiveExpert, lingo.PerspectiveLeader, ""}[i%4]
			u := lingo.User{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("人员%d", i), Role: lingo.RoleExternal, Perspective: p}
			_, err := l.AddPerspective(ctx, u, tk.ID, fmt.Sprintf("并发补充-%d", i))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		var pending int
		_ = db.QueryRow("SELECT count(*) FROM lingo_outbox WHERE kind = 'gitea' AND status <> 'done'").Scan(&pending)
		g.mu.Lock()
		defer g.mu.Unlock()
		return pending == 0 && len(g.comments) == n
	})
	g.mu.Lock()
	body, _ := g.issues[0]["body"].(string)
	edits := len(g.edits)
	g.mu.Unlock()
	ps, err := l.ListPerspectives(ctx, rita, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, gr := range ps.Groups {
		for _, e := range gr.Entries {
			total++
			if !e.Original && strings.Count(body, "> "+e.Content+"\n") != 1 {
				t.Errorf("issue body has %q %d times", e.Content, strings.Count(body, "> "+e.Content+"\n"))
			}
		}
	}
	if total != n+1 || strings.Count(body, "<!-- lingo:perspectives -->") != 1 || strings.Contains(body, "缺少") || len(ps.Missing) != 0 {
		t.Errorf("entries %d, missing %v, body:\n%s", total, ps.Missing, body)
	}

	// lingo 自己改正文触发的 edited Webhook：不新增视角数据，也不再重写正文
	var rows, tasks int
	_ = db.QueryRow("SELECT count(*) FROM lingo_ticket_perspectives").Scan(&rows)
	_ = db.QueryRow("SELECT count(*) FROM lingo_outbox WHERE kind = 'gitea'").Scan(&tasks)
	payload, _ := json.Marshal(map[string]any{"action": "edited",
		"issue":      map[string]any{"id": 1000, "number": 40, "state": "open", "body": body, "user": map[string]any{"login": "lingo-bot"}, "updated_at": "2026-11-21T10:00:00Z"},
		"changes":    map[string]any{"body": map[string]any{"from": created}},
		"repository": map[string]any{"full_name": "demo/api"}, "sender": map[string]any{"login": "lingo-bot"}})
	if w := deliver(l.WebhookHandler(), "p-edit-1", "issues", string(payload), "s"); w.Code != 200 {
		t.Fatalf("deliver = %d %s", w.Code, w.Body)
	}
	waitProcessed(t, db, 1)
	var rows2, tasks2 int
	_ = db.QueryRow("SELECT count(*) FROM lingo_ticket_perspectives").Scan(&rows2)
	_ = db.QueryRow("SELECT count(*) FROM lingo_outbox WHERE kind = 'gitea'").Scan(&tasks2)
	g.mu.Lock()
	edits2 := len(g.edits)
	g.mu.Unlock()
	if rows2 != rows || tasks2 != tasks || edits2 != edits {
		t.Errorf("edited webhook changed state: rows %d→%d tasks %d→%d edits %d→%d", rows, rows2, tasks, tasks2, edits, edits2)
	}

	// 视角事件：提交人看得到本人工单的，其他 external 看不到
	count := func(u lingo.User) int {
		evs, err := l.ListEvents(ctx, u, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		c := 0
		for _, e := range evs {
			if e.Kind == lingo.EventTicketPerspectiveAdded || e.Kind == lingo.EventTicketPerspectiveMissing {
				c++
			}
		}
		return c
	}
	waitFor(t, func() bool { return count(rita) == n+1 })
	if got := count(alice); got != n+1 {
		t.Errorf("owner sees %d perspective events", got)
	}
	if got := count(lingo.User{ID: "c1", Role: lingo.RoleExternal}); got != 0 {
		t.Errorf("other external sees %d perspective events", got)
	}
}

func TestPerspectivesMCP(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	l, _ := newPerspectiveLingo(t, g)
	tk, err := l.CreateTicket(context.Background(), alice, lingo.TicketInput{Type: lingo.TicketFeature, Content: "导出"})
	if err != nil {
		t.Fatal(err)
	}
	h := l.MCPHandler()
	call := func(user, persp, tool string, args any) (map[string]any, bool) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(b)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User", user)
		r.Header.Set("X-Role", "external")
		r.Header.Set("X-Perspective", persp)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var resp struct {
			Result struct {
				IsError           bool           `json:"isError"`
				StructuredContent map[string]any `json:"structuredContent"`
				Content           []struct{ Text string }
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: %d %s", tool, w.Code, w.Body)
		}
		if resp.Result.IsError {
			return map[string]any{"error": resp.Result.Content[0].Text}, false
		}
		return resp.Result.StructuredContent, true
	}
	added, ok := call("b1", "business", "add_perspective", map[string]any{"id": tk.ID, "content": "影响 30 家门店"})
	if !ok || added["perspective"] != "business" || added["author_name"] != "b1" || added["content"] != "影响 30 家门店" {
		t.Fatalf("add_perspective = %v", added)
	}
	if out, ok := call("b1", "boss", "add_perspective", map[string]any{"id": tk.ID, "content": "x"}); ok || !strings.Contains(out["error"].(string), "invalid input") {
		t.Errorf("bad perspective = %v", out)
	}
	if out, ok := call("b1", "business", "add_perspective", map[string]any{"content": "x"}); ok || !strings.Contains(out["error"].(string), "id is required") {
		t.Errorf("missing id = %v", out)
	}
	list, ok := call("e1", "expert", "list_perspectives", map[string]any{"id": tk.ID})
	if !ok {
		t.Fatalf("list_perspectives = %v", list)
	}
	// 与 Go 方法结果一致
	want, err := l.ListPerspectives(context.Background(), rita, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(want)
	var wantMap map[string]any
	_ = json.Unmarshal(wantJSON, &wantMap)
	if jsonString(list) != jsonString(wantMap) {
		t.Errorf("mcp = %s\ngo  = %s", jsonString(list), jsonString(wantMap))
	}
	if gs := list["groups"].([]any); len(gs) != 2 || list["missing"].([]any)[0] != "leader" {
		t.Errorf("groups/missing = %v", list)
	}
}
