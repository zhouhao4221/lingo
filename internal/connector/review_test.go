package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// PR #50 审查项的回归测试。

func TestBusinessOutcomesNotCountedByBreaker(t *testing.T) {
	var next error
	e := newEnv(t, spec("get_order", func(context.Context, Caller, json.RawMessage) (Output, error) { return Output{Data: 1}, next }))
	in := json.RawMessage(`{"order_id":"1024","undeclared":"x"}`)
	run := func() error {
		_, err := e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0)
		return err
	}
	for i := 0; i < 2*failThreshold; i++ {
		next = fmt.Errorf("order 1024 belongs to someone else: %w", ErrDenied)
		if i%2 == 1 {
			next = fmt.Errorf("order not in system: %w", ErrNoData)
		}
		err := run()
		if !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), "1024") {
			t.Fatalf("%d: err = %v", i, err)
		}
		r := e.audit.last()
		want := ReasonBizDenied
		if i%2 == 1 {
			want = ReasonNoData
		}
		if r.Status != StatusDenied || r.Reason != want || r.Args != nil || len(r.ArgNames) != 1 {
			t.Fatalf("%d: record = %+v", i, r)
		}
	}
	next = nil
	if err := run(); err != nil {
		t.Fatalf("business outcomes opened the breaker: %v", err)
	}
	// 技术故障照常计入：连续 5 次后暂停
	next = errors.New("connection refused")
	for i := 0; i < failThreshold; i++ {
		_ = run()
	}
	if err := run(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("technical failures did not open: %v", err)
	}
	// 试探调用得到业务结果：不算恢复也不算失败，下一次重新试探
	e.now = e.now.Add(61 * time.Second)
	next = ErrNoData
	if err := run(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("probe: %v", err)
	}
	next = nil
	if err := run(); err != nil {
		t.Fatalf("second probe not allowed: %v", err)
	}
	if l := e.logs.String(); strings.Contains(l, "1024") || strings.Contains(l, "someone else") {
		t.Errorf("logs leak: %s", l)
	}
}

func TestScopeValidation(t *testing.T) {
	for _, sc := range []string{"order:read ", " order:read", "order read", "order:read\n", "order:\tread"} {
		s := spec("get_order", okCall(1))
		s.Scope = sc
		ts, rej := register([]Spec{s}, nil)
		if len(ts) != 0 || len(rej) != 1 || rej[0].Reason != RejectScope {
			t.Errorf("scope %q: tools=%d rej=%v", sc, len(ts), rej)
		}
	}
	if ts, _ := register([]Spec{spec("get_order", okCall(1))}, nil); len(ts) != 1 {
		t.Error("valid scope rejected")
	}
}

func TestIntegerAndErrorOrder(t *testing.T) {
	sc, err := parseSchema(json.RawMessage(`{"type":"object","properties":{"b":{"type":"integer"},"a":{"type":"integer"},"c":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	// integer 与 Go 标准整型解码一致（契约见 TestIntegerMatchesGoDecoding）：3.0、1e3 不再接受
	for _, in := range []string{`{"a":3}`, `{"a":-2}`, `{"a":0}`} {
		if err := sc.validate(decodeObject(json.RawMessage(in))); err != nil {
			t.Errorf("%s rejected: %v", in, err)
		}
	}
	for _, in := range []string{`{"a":3.5}`, `{"a":"3"}`, `{"a":true}`, `{"a":3.0}`, `{"a":1e3}`} {
		if err := sc.validate(decodeObject(json.RawMessage(in))); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
	for i := 0; i < 20; i++ {
		var ie *InputError
		if err := sc.validate(decodeObject(json.RawMessage(`{"c":1,"b":1.5,"a":"x"}`))); !errors.As(err, &ie) || ie.Param != "a" {
			t.Fatalf("error order not deterministic: %v", err)
		}
	}
}

func TestMaskCaseInsensitive(t *testing.T) {
	type cust struct{ Phone, Name string }
	data := struct {
		Phone    string
		Customer cust `json:"Customer"`
		Items    []cust
	}{"13800000000", cust{"13900000000", "张三"}, []cust{{"13700000000", "李四"}}}
	out, _, _, err := encodeResult(data, parsePaths([]string{"phone", "customer.PHONE", "items.*.phone"}))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "138") || strings.Contains(s, "139") || strings.Contains(s, "137") || !strings.Contains(s, "张三") {
		t.Errorf("masked = %s", s)
	}
	sp := spec("get_order", okCall(1))
	sp.Sensitive = []string{"ORDER_ID"}
	e := newEnv(t, sp)
	if _, err := e.s.Run(context.Background(), alice, "get_order", json.RawMessage(`{"order_id":"SECRET"}`), ChannelAPI, 0); err != nil {
		t.Fatal(err)
	}
	if r := e.audit.last(); r.Args["order_id"] != "***" {
		t.Errorf("args = %v", r.Args)
	}
}
