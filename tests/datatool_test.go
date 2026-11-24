package tests

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// 业务数据工具（REQ-008）的集成测试：审计落库、可见范围、中断恢复。

func scopedHeaderUser(r *http.Request) (lingo.User, error) {
	if r.Header.Get("X-User") == "" {
		return lingo.User{}, errors.New("anonymous")
	}
	u := lingo.User{ID: r.Header.Get("X-User"), Name: r.Header.Get("X-User"), Role: lingo.Role(r.Header.Get("X-Role"))}
	if s := r.Header.Get("X-Scopes"); s != "" {
		u.Scopes = strings.Split(s, ",")
	}
	if p := r.Header.Get("X-Projects"); p != "" {
		u.Projects = strings.Split(p, ",")
	}
	return u, nil
}

var toolProjects = []lingo.Project{
	{Key: "crm", Name: "CRM", Repos: []lingo.Repo{{Owner: "demo", Name: "crm", Kind: lingo.RepoBackend}}},
	{Key: "hr", Name: "HR", Repos: []lingo.Repo{{Owner: "demo", Name: "hr", Kind: lingo.RepoBackend}}},
}

const orderSchema = `{"type":"object","required":["order_id"],"properties":{"order_id":{"type":"string","description":"订单号"}}}`

func newToolLingo(t *testing.T, db *sql.DB, tools ...lingo.Tool) *lingo.Lingo {
	t.Helper()
	l, err := lingo.New(lingo.Config{DB: db, Model: nopModel{}, Projects: toolProjects, Authenticate: scopedHeaderUser, Tools: tools,
		SkipSelfCheck: true, ReconcileInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func orderTool(project string) lingo.Tool {
	return lingo.Tool{Name: "get_order", Description: "按订单号查询订单", InputSchema: json.RawMessage(orderSchema),
		Scope: "order:read", Mode: lingo.ToolRead, Sensitive: []string{"customer.phone"},
		Call: func(_ context.Context, u lingo.User, in json.RawMessage) (any, error) {
			var p struct {
				OrderID string `json:"order_id"`
			}
			_ = json.Unmarshal(in, &p)
			data := map[string]any{"id": p.OrderID, "owner": u.ID, "customer": map[string]any{"phone": "13800000000"}}
			if project == "" {
				return data, nil
			}
			return lingo.ToolOutput{Project: project, Data: data}, nil
		}}
}

func TestDataToolAuditScope(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	crm := newToolLingo(t, db, orderTool("crm"))
	alice := lingo.User{ID: "alice", Role: lingo.RoleExternal, Scopes: []string{"order:read"}, Projects: []string{"crm"}}
	res, err := crm.CallTool(ctx, alice, "get_order", json.RawMessage(`{"order_id":"1024"}`))
	if err != nil || res.Truncated || strings.Contains(string(res.Data), "13800000000") || !strings.Contains(string(res.Data), `"owner":"alice"`) {
		t.Fatalf("CallTool = %s, %v", res.Data, err)
	}
	// 未声明归属的调用（另一个实例注册的同名工具不声明项目）
	plain := newToolLingo(t, db, orderTool(""))
	bob := lingo.User{ID: "bob", Role: lingo.RoleExternal, Scopes: []string{"order:read"}, Projects: []string{"crm"}}
	if _, err := plain.CallTool(ctx, bob, "get_order", json.RawMessage(`{"order_id":"2048"}`)); err != nil {
		t.Fatal(err)
	}
	// 拒绝：参数不合法，记录只含参数名
	if _, err := crm.CallTool(ctx, bob, "get_order", json.RawMessage(`{"order_id":2048,"note":"SECRET"}`)); !errors.Is(err, lingo.ErrInvalidInput) ||
		strings.Contains(err.Error(), "2048") {
		t.Fatalf("invalid input: %v", err)
	}
	// 声明的项目调用人无权访问：与工具不存在相同
	hrOnly := lingo.User{ID: "carol", Role: lingo.RoleExternal, Scopes: []string{"order:read"}, Projects: []string{"hr"}}
	_, errProject := crm.CallTool(ctx, hrOnly, "get_order", json.RawMessage(`{"order_id":"1"}`))
	_, errMissing := crm.CallTool(ctx, hrOnly, "no_such_tool", nil)
	if !errors.Is(errProject, lingo.ErrNotFound) || errProject.Error() != errMissing.Error() {
		t.Fatalf("project denial = %v, missing = %v", errProject, errMissing)
	}

	ids := func(u lingo.User, f lingo.DataRequestFilter) map[string]int {
		t.Helper()
		rs, err := crm.ListDataRequests(ctx, u, f)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, r := range rs {
			out[r.UserID+"/"+r.Status]++
			if r.Status == "denied" && r.Args != nil {
				t.Errorf("denied record has values: %+v", r)
			}
			if r.UserID == "carol" && r.Project != "" {
				t.Errorf("denied record keeps project: %+v", r)
			}
			if b, _ := json.Marshal(r); strings.Contains(string(b), "13800000000") || strings.Contains(string(b), "SECRET") {
				t.Errorf("record leaks data: %s", b)
			}
		}
		return out
	}
	reviewerCRM := lingo.User{ID: "rev", Role: lingo.RoleReviewer, Projects: []string{"crm"}}
	reviewerAll := lingo.User{ID: "rev2", Role: lingo.RoleReviewer}
	adminHR := lingo.User{ID: "adm", Role: lingo.RoleAdmin, Projects: []string{"hr"}}
	agent := lingo.User{ID: "bot", Role: lingo.RoleAgent, Projects: []string{"crm"}}
	cases := []struct {
		name string
		u    lingo.User
		f    lingo.DataRequestFilter
		want map[string]int
	}{
		{"owner", alice, lingo.DataRequestFilter{}, map[string]int{"alice/ok": 1}},
		{"bob sees own incl. denied", bob, lingo.DataRequestFilter{}, map[string]int{"bob/ok": 1, "bob/denied": 1}},
		{"reviewer of crm sees attributed only", reviewerCRM, lingo.DataRequestFilter{}, map[string]int{"alice/ok": 1}},
		{"reviewer with empty projects sees none", reviewerAll, lingo.DataRequestFilter{}, map[string]int{}},
		{"admin of hr sees none", adminHR, lingo.DataRequestFilter{}, map[string]int{}},
		{"agent sees none", agent, lingo.DataRequestFilter{}, map[string]int{}},
		{"filter out-of-scope user", reviewerCRM, lingo.DataRequestFilter{UserID: "bob"}, map[string]int{}},
		{"filter by status", bob, lingo.DataRequestFilter{Status: "denied"}, map[string]int{"bob/denied": 1}},
		{"carol sees own denied", hrOnly, lingo.DataRequestFilter{}, map[string]int{"carol/denied": 2}},
	}
	for _, c := range cases {
		got := ids(c.u, c.f)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
	if _, err := crm.ListDataRequests(ctx, alice, lingo.DataRequestFilter{Status: "bogus"}); !errors.Is(err, lingo.ErrInvalidInput) {
		t.Errorf("bad status: %v", err)
	}
	if rs, _ := crm.ListDataRequests(ctx, alice, lingo.DataRequestFilter{}); len(rs) != 1 || rs[0].Project != "crm" || rs[0].Args["order_id"] != "1024" ||
		rs[0].Channel != "api" || rs[0].FinishedAt == nil {
		t.Errorf("alice record = %+v", rs)
	}
}

func TestDataToolInterruptRecovery(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	// 过期与未过期的待完成记录（模拟崩溃的实例与仍在执行的实例）
	if _, err := db.ExecContext(ctx, `INSERT INTO lingo_data_requests (user_id, user_role, tool, channel, status, lease_until) VALUES
		('u1', 'external', 'get_order', 'api', 'pending', now() - interval '1 minute'),
		('u1', 'external', 'get_order', 'api', 'pending', now() - interval '1 second'),
		('u1', 'external', 'get_order', 'api', 'pending', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	// 另一个实例上正在执行的长超时调用（Timeout 60 秒，租约 3 分钟）
	release := make(chan struct{})
	started := make(chan struct{})
	slow := orderTool("")
	slow.Name, slow.Timeout = "slow_order", 60*time.Second
	inner := slow.Call
	slow.Call = func(ctx context.Context, u lingo.User, in json.RawMessage) (any, error) {
		close(started)
		<-release
		return inner(ctx, u, in)
	}
	busy := newToolLingo(t, db, slow)
	alice := lingo.User{ID: "alice", Role: lingo.RoleExternal, Scopes: []string{"order:read"}}
	callDone := make(chan error, 1)
	go func() {
		_, err := busy.CallTool(ctx, alice, "slow_order", json.RawMessage(`{"order_id":"1"}`))
		callDone <- err
	}()
	<-started

	// 两个实例同时执行恢复
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		l := newToolLingo(t, db, orderTool(""))
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			_ = l.Run(rctx)
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	var interrupted int
	for time.Now().Before(deadline) {
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM lingo_data_requests WHERE status = 'interrupted'").Scan(&interrupted)
		if interrupted >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	wg.Wait()
	var pending int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM lingo_data_requests WHERE status = 'pending'").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if interrupted != 2 || pending != 2 {
		t.Fatalf("interrupted = %d (want 2), pending = %d (want 2: 未过期的与正在执行的)", interrupted, pending)
	}
	close(release)
	if err := <-callDone; err != nil {
		t.Fatalf("in-flight call: %v", err)
	}
	var status string
	if err := db.QueryRowContext(ctx, "SELECT status FROM lingo_data_requests WHERE tool = 'slow_order'").Scan(&status); err != nil || status != "ok" {
		t.Errorf("in-flight record = %q, %v", status, err)
	}
}

func TestDataToolPurge(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO lingo_data_requests (user_id, user_role, tool, channel, status, created_at) VALUES
		('u1', 'external', 'get_order', 'api', 'ok', now() - interval '181 days'),
		('u1', 'external', 'get_order', 'api', 'ok', now() - interval '179 days')`); err != nil {
		t.Fatal(err)
	}
	l := newToolLingo(t, db, orderTool(""))
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = l.Run(rctx)
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM lingo_data_requests").Scan(&n); err != nil || n != 1 {
		t.Errorf("rows after purge = %d, %v", n, err)
	}
}

func mcpRaw(t *testing.T, h http.Handler, hdr map[string]string, body string) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Body.String()
}

const toolsListReq = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// 回归：不注册工具或注册的全部被忽略时，tools/list 与 v0.8.0 完全一致（testdata 由 v0.8.0 生成），initialize 不变。
func TestDataToolMCPRegression(t *testing.T) {
	golden, err := os.ReadFile("testdata/mcp_tools_v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	db := testDB(t, true)
	user := map[string]string{"X-User": "u1", "X-Role": "external", "X-Scopes": "order:read"}
	ignored := []lingo.Tool{
		func() lingo.Tool { x := orderTool(""); x.Mode = lingo.ToolWrite; return x }(),
		func() lingo.Tool { x := orderTool(""); x.Scope = ""; return x }(),
		func() lingo.Tool { x := orderTool(""); x.Name = "ASK"; return x }(),
		func() lingo.Tool { x := orderTool(""); x.Timeout = 2 * time.Minute; return x }(),
	}
	none := newToolLingo(t, db)
	bad := newToolLingo(t, db, ignored...)
	for name, l := range map[string]*lingo.Lingo{"none": none, "all ignored": bad} {
		if got := mcpRaw(t, l.MCPHandler(), user, toolsListReq); got != string(golden) {
			t.Errorf("%s: tools/list differs from v0.8.0", name)
		}
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
	if a, b := mcpRaw(t, none.MCPHandler(), user, init), mcpRaw(t, bad.MCPHandler(), user, init); a != b {
		t.Errorf("initialize differs:\n%s\n%s", a, b)
	}
}

func TestDataToolMCP(t *testing.T) {
	golden, _ := os.ReadFile("testdata/mcp_tools_v0.8.0.json")
	db := testDB(t, true)
	ctx := context.Background()
	hrTool := orderTool("hr")
	hrTool.Name = "get_hr_order"
	l := newToolLingo(t, db, orderTool(""), hrTool)
	h := l.MCPHandler()
	scoped := map[string]string{"X-User": "u1", "X-Role": "agent", "X-Scopes": "order:read", "X-Projects": "crm"}
	plain := map[string]string{"X-User": "u2", "X-Role": "agent"}
	if got := mcpRaw(t, h, plain, toolsListReq); got != string(golden) {
		t.Error("caller without scope sees business tools")
	}
	list := mcpRaw(t, h, scoped, toolsListReq)
	if !strings.Contains(list, `"name":"get_order"`) || !strings.Contains(list, `"name":"list_projects"`) {
		t.Errorf("tools/list = %s", list)
	}
	call := func(hdr map[string]string, name, args string) string {
		return mcpRaw(t, h, hdr, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	}
	ok := call(scoped, "get_order", `{"order_id":"1024"}`)
	if !strings.Contains(ok, `\"owner\":\"u1\"`) || strings.Contains(ok, "13800000000") || !strings.Contains(ok, `"structuredContent"`) {
		t.Errorf("call = %s", ok)
	}
	missing := strings.ReplaceAll(call(plain, "no_such_tool", `{}`), "no_such_tool", "X")
	if got := strings.ReplaceAll(call(plain, "get_order", `{"order_id":"1"}`), "get_order", "X"); got != missing {
		t.Errorf("no-scope call differs from missing tool:\n%s\n%s", got, missing)
	}
	// 结果声明的项目（hr）调用人无权访问：与工具不存在相同
	if got := strings.ReplaceAll(call(scoped, "get_hr_order", `{"order_id":"1"}`), "get_hr_order", "X"); got != missing {
		t.Errorf("project denial differs from missing tool:\n%s\n%s", got, missing)
	}
	bad := call(scoped, "get_order", `{"order_id":98765}`)
	if !strings.Contains(bad, "invalid arguments: parameter order_id must be string") || strings.Contains(bad, "98765") {
		t.Errorf("invalid args = %s", bad)
	}
	rs, err := l.ListDataRequests(ctx, lingo.User{ID: "u1", Role: lingo.RoleAgent}, lingo.DataRequestFilter{})
	if err != nil || len(rs) != 3 {
		t.Fatalf("audit = %+v, %v", rs, err)
	}
	for _, r := range rs {
		if r.Channel != "mcp" || r.Project != "" {
			t.Errorf("mcp record = %+v", r)
		}
	}
}
