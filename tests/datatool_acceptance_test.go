package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo"
)

// REQ-008 验收（6.2）：只用本地临时 schema、虚构业务数据与脚本化模型，不调用任何真实业务系统或模型服务。

// 虚构的请假系统：每人一条余额，含不应外发的身份证号。
var fakeLeave = map[string]map[string]any{
	"emp-001": {"employee": "emp-001", "annual_leave_days": 5.5, "comp_off_days": 1, "id_card": "110101199001011234"},
	"emp-002": {"employee": "emp-002", "annual_leave_days": 12, "comp_off_days": 0, "id_card": "110101199202022345"},
	"erin":    {"employee": "erin", "annual_leave_days": 3, "comp_off_days": 0, "id_card": "110101199303033456"},
}

func leaveTool(called *[]string) lingo.Tool {
	return lingo.Tool{Name: "get_leave_balance", Description: "查询本人的剩余年假与调休天数",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`), Scope: "leave:read", Mode: lingo.ToolRead,
		Sensitive: []string{"id_card"},
		Call: func(_ context.Context, u lingo.User, _ json.RawMessage) (any, error) {
			*called = append(*called, u.ID)
			b, ok := fakeLeave[u.ID] // 只查本人（二次鉴权：以传入用户为准，不接受参数指定他人）
			if !ok {
				return nil, errors.New("no such employee")
			}
			return lingo.ToolOutput{Project: "hr", Data: b}, nil
		}}
}

func TestDataToolAcceptance(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	var leaveCalls []string
	logs := &bytes.Buffer{}
	m := &toolModel{}
	tools := []lingo.Tool{
		orderTool("crm"),
		leaveTool(&leaveCalls),
		func() lingo.Tool { x := orderTool(""); x.Name = "ask"; return x }(),                // 与内置工具同名：忽略
		func() lingo.Tool { x := leaveTool(&leaveCalls); x.Description = "冒名"; return x }(), // 与先注册的同名：忽略
		func() lingo.Tool { x := orderTool(""); x.Name = "pay_order"; x.Mode = lingo.ToolWrite; return x }(),
	}
	l, err := lingo.New(lingo.Config{DB: db, Model: m, Projects: toolProjects, Authenticate: scopedHeaderUser, Tools: tools,
		SkipSelfCheck: true, ReconcileInterval: -1, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatalf("验收 5：配置有误的工具让启动失败：%v", err)
	}

	ask := func(u lingo.User, intent, answer, q string) lingo.ConversationMessage {
		t.Helper()
		c, err := l.CreateConversation(ctx, u, "")
		if err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		m.intents, m.answer = []string{intent}, answer
		m.mu.Unlock()
		msg, err := l.SendMessage(ctx, u, c.ID, lingo.MessageInput{Content: q})
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}
	lastAnswerPrompt := func() string {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.calls[len(m.calls)-1][1]
	}

	// 验收 1：有 order:read 的用户问订单，得到基于订单数据的回答并看到所用工具
	alice := lingo.User{ID: "alice", Role: lingo.RoleExternal, Scopes: []string{"order:read"}, Projects: []string{"crm"}}
	msg := ask(alice, `{"intent":"data_query","tool":"get_order","tool_input":{"order_id":"1024"}}`, "订单 1024 在财务审批环节被退回。", "订单 1024 为什么审批失败")
	if len(msg.Citations.Tools) != 1 || msg.Citations.Tools[0].Name != "get_order" || !strings.Contains(lastAnswerPrompt(), `\"id\":\"1024\"`) {
		t.Errorf("验收 1：%+v", msg)
	}

	// 验收 2：没有对应权限的用户无法让灵工查到数据（模型看不到工具；即使模型给出工具也不调用）
	nobody := lingo.User{ID: "dave", Role: lingo.RoleExternal, Projects: []string{"crm"}}
	before := len(m.intentCalls())
	r := ask(nobody, `{"intent":"data_query","tool":"get_order","tool_input":{"order_id":"1024"}}`, "不应出现", "订单 1024 为什么审批失败")
	if calls := m.intentCalls(); strings.Contains(calls[len(calls)-1][1], "get_order") || len(calls) != before+1 {
		t.Error("验收 2：无权限用户的意图解析看到了工具")
	}
	if len(r.Citations.Tools) != 0 || strings.Contains(r.Content, "1024 在") {
		t.Errorf("验收 2：%+v", r)
	}
	if _, err := l.CallTool(ctx, nobody, "get_order", json.RawMessage(`{"order_id":"1024"}`)); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("验收 2：CallTool = %v", err)
	}

	// 验收 3：请假余额只得到本人的；敏感字段不出现在回答输入与 MCP 结果中
	emp1 := lingo.User{ID: "emp-001", Role: lingo.RoleExternal, Scopes: []string{"leave:read"}, Projects: []string{"hr"}}
	msg = ask(emp1, `{"intent":"data_query","tool":"get_leave_balance","tool_input":{"employee":"emp-002"}}`, "你还有 5.5 天年假、1 天调休。", "我的请假余额还有几天")
	p := lastAnswerPrompt()
	if msg.Citations.Tools[0].Name != "get_leave_balance" || !strings.Contains(p, "5.5") || strings.Contains(p, "emp-002") ||
		strings.Contains(p, "110101199001011234") || !strings.Contains(p, "***") {
		t.Errorf("验收 3：问答 %+v\n%s", msg, p)
	}
	h := l.MCPHandler()
	out := mcpRaw(t, h, map[string]string{"X-User": "emp-002", "X-Role": "external", "X-Scopes": "leave:read", "X-Projects": "hr"},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_leave_balance","arguments":{}}}`)
	if !strings.Contains(out, "annual_leave_days") || !strings.Contains(out, "12") || strings.Contains(out, "110101199202022345") || strings.Contains(out, "5.5") {
		t.Errorf("验收 3：MCP %s", out)
	}
	if len(leaveCalls) < 2 || leaveCalls[0] != "emp-001" || leaveCalls[1] != "emp-002" {
		t.Errorf("验收 3：Call 收到的用户 = %v", leaveCalls)
	}
	var stored int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM lingo_assistant_messages WHERE content LIKE '%1101011990%' OR citations::text LIKE '%annual_leave%'`).Scan(&stored)
	if stored != 0 {
		t.Error("验收 3：原始结果进入了会话")
	}

	// 验收 4：负责项目的 reviewer 看到归属该项目的调用；用户只看本人；未明确负责项目的 reviewer 看不到他人
	count := func(u lingo.User) map[string]int {
		rs, err := l.ListDataRequests(ctx, u, lingo.DataRequestFilter{})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, r := range rs {
			out[r.UserID+"/"+r.Tool+"/"+r.Status]++
		}
		return out
	}
	hrReviewer := lingo.User{ID: "rev-hr", Role: lingo.RoleReviewer, Projects: []string{"hr"}}
	crmReviewer := lingo.User{ID: "rev-crm", Role: lingo.RoleReviewer, Projects: []string{"crm"}}
	openReviewer := lingo.User{ID: "rev-any", Role: lingo.RoleReviewer}
	if got := count(hrReviewer); got["emp-001/get_leave_balance/ok"] != 1 || got["emp-002/get_leave_balance/ok"] != 1 || got["alice/get_order/ok"] != 0 {
		t.Errorf("验收 4：hr reviewer = %v", got)
	}
	if got := count(crmReviewer); got["alice/get_order/ok"] != 1 || len(got) != 1 {
		t.Errorf("验收 4：crm reviewer = %v", got)
	}
	if got := count(emp1); len(got) != 1 || got["emp-001/get_leave_balance/ok"] != 1 {
		t.Errorf("验收 4：本人 = %v", got)
	}
	if got := count(openReviewer); len(got) != 0 {
		t.Errorf("验收 4：Projects 为空的 reviewer = %v", got)
	}

	// 验收 5：配置有误的工具不让启动失败，也不替换已有工具
	for _, want := range []string{"index=2 name=ask reason=builtin_name", "index=3 name=get_leave_balance reason=duplicate_name", "index=4 name=pay_order reason=write_mode"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("验收 5：缺少告警 %q", want)
		}
	}
	list := mcpRaw(t, h, map[string]string{"X-User": "emp-001", "X-Role": "external", "X-Scopes": "leave:read,order:read"}, toolsListReq)
	if strings.Contains(list, "冒名") || strings.Contains(list, "pay_order") || strings.Count(list, `"name":"ask"`) != 1 ||
		!strings.Contains(list, "以自然语言询问工单进展") {
		t.Errorf("验收 5：tools/list 被替换或包含被忽略的工具")
	}

	// 验收 6：声明的归属项目用户无权访问时拿不到数据，也看不出项目是否存在
	crmOnly := lingo.User{ID: "erin", Role: lingo.RoleExternal, Scopes: []string{"leave:read"}, Projects: []string{"crm"}}
	_, errDenied := l.CallTool(ctx, crmOnly, "get_leave_balance", nil)
	_, errMissing := l.CallTool(ctx, crmOnly, "get_nothing", nil)
	if !errors.Is(errDenied, lingo.ErrNotFound) || errDenied.Error() != errMissing.Error() {
		t.Errorf("验收 6：%v / %v", errDenied, errMissing)
	}
	r = ask(crmOnly, `{"intent":"data_query","tool":"get_leave_balance","tool_input":{}}`, "不应出现", "我的请假余额还有几天")
	if r.Content != "没有查到相关的业务数据。" || strings.Contains(r.Content, "hr") || strings.Contains(lastAnswerPrompt(), "110101199303033456") {
		t.Errorf("验收 6：问答回复 %q", r.Content)
	}
	rs, _ := l.ListDataRequests(ctx, crmOnly, lingo.DataRequestFilter{})
	for _, x := range rs {
		if x.Tool == "get_leave_balance" && (x.Status != "denied" || x.Reason != "forbidden" || x.Project != "" || x.Args != nil) {
			t.Errorf("验收 6：拒绝记录 %+v", x)
		}
	}
}
