package lingo_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zhouhao4221/lingo"
)

// 接入方自己的业务系统（示意）。
type order struct {
	ID         string `json:"id"`
	OwnerID    string `json:"-"`
	ProjectKey string `json:"-"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Customer   struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
	} `json:"customer"`
}

var orders = map[string]order{"1024": {ID: "1024", OwnerID: "u100", ProjectKey: "demo", Status: "rejected", Reason: "发票抬头不符"}}

func canViewAllOrders(u lingo.User) bool { return u.Role == lingo.RoleAdmin }

// 注册只读业务数据工具：灵工按 User.Scopes 预校验，Call 内必须按传入用户再次鉴权。
func ExampleTool() {
	orderTool := lingo.Tool{
		Name:        "get_order",
		Description: "按订单号查询订单状态与审批记录",
		InputSchema: json.RawMessage(`{"type":"object","required":["order_id"],
			"properties":{"order_id":{"type":"string","description":"订单号"}}}`),
		Scope:     "order:read",
		Mode:      lingo.ToolRead,
		Sensitive: []string{"customer.phone"},
		Timeout:   5 * time.Second,
		Call: func(ctx context.Context, u lingo.User, in json.RawMessage) (any, error) {
			var p struct {
				OrderID string `json:"order_id"`
			}
			if err := json.Unmarshal(in, &p); err != nil {
				return nil, err
			}
			o, ok := orders[p.OrderID]
			if !ok {
				return nil, lingo.ErrNotFound // 查无数据：正常业务结果，不计入熔断
			}
			if o.OwnerID != u.ID && !canViewAllOrders(u) { // 二次鉴权
				return nil, lingo.ErrForbidden // 业务拒绝：正常业务结果，不计入熔断
			}
			return lingo.ToolOutput{Project: o.ProjectKey, Data: o}, nil // 声明归属项目，供负责该项目的 reviewer 审计
		},
	}
	leaveTool := lingo.Tool{
		Name: "get_leave_balance", Description: "查询本人的剩余年假与调休天数",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Scope:       "leave:read", Mode: lingo.ToolRead,
		Call: func(ctx context.Context, u lingo.User, _ json.RawMessage) (any, error) {
			return map[string]any{"user": u.ID, "annual_leave_days": 5.5}, nil // 只查本人
		},
	}
	cfg := lingo.Config{ /* DB、Projects、Authenticate 等 */ Tools: []lingo.Tool{orderTool, leaveTool}}
	fmt.Println(len(cfg.Tools))
	// Output: 2
}

// 接入方界面直接调用，并查看审计。
func ExampleLingo_CallTool() {
	var l *lingo.Lingo // lingo.New(cfg)
	if l == nil {
		return
	}
	ctx := context.Background()
	u := lingo.User{ID: "u100", Role: lingo.RoleExternal, Scopes: []string{"order:read"}, Projects: []string{"demo"}}
	res, err := l.CallTool(ctx, u, "get_order", json.RawMessage(`{"order_id":"1024"}`))
	switch {
	case errors.Is(err, lingo.ErrNotFound): // 不存在、无权限或归属项目不可访问（不区分）
	case errors.Is(err, lingo.ErrInvalidInput): // 参数不合法（错误只含参数名）
	case errors.Is(err, lingo.ErrToolUnavailable), errors.Is(err, lingo.ErrToolFailed):
	case err == nil:
		fmt.Println(string(res.Data), res.Truncated)
	}
	reviewer := lingo.User{ID: "r1", Role: lingo.RoleReviewer, Projects: []string{"demo"}} // 只看明确负责的项目
	rs, _ := l.ListDataRequests(ctx, reviewer, lingo.DataRequestFilter{Tool: "get_order", Since: time.Now().Add(-24 * time.Hour)})
	fmt.Println(len(rs))
}
