package connector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// 输入完整性与整型契约（PR #50 复核）。

func TestInputMustBeOneCompleteObject(t *testing.T) {
	called := 0
	e := newEnv(t, spec("get_order", func(_ context.Context, _ Caller, in json.RawMessage) (Output, error) {
		called++
		var p struct {
			OrderID string `json:"order_id"`
		}
		if err := json.Unmarshal(in, &p); err != nil { // 接入方按标准方式解码
			return Output{}, err
		}
		return Output{Data: p.OrderID}, nil
	}))
	for _, in := range []string{
		`{"order_id":"1"} trailing`,
		`{"order_id":"1"}{"order_id":"2"}`,
		`{"order_id":"1"} []`,
		`{"order_id":"1"`,
		`[{"order_id":"1"}]`,
		`"order_id"`,
		`not json`,
	} {
		_, err := e.s.Run(context.Background(), alice, "get_order", json.RawMessage(in), ChannelAPI, 0)
		var ie *InputError
		if !errors.As(err, &ie) || ie.Param != "" {
			t.Errorf("%q: err = %v", in, err)
		}
		if r := e.audit.last(); r.Status != StatusDenied || r.Reason != ReasonInvalidInput || r.Args != nil || len(r.ArgNames) != 0 {
			t.Errorf("%q: record = %+v", in, r)
		}
	}
	if called != 0 {
		t.Fatalf("Call invoked for malformed input %d times", called)
	}
	// 参数错误不计入熔断：之后的合法调用照常执行
	for _, in := range []string{`{"order_id":"1"}`, `  {"order_id":"1"}  `, `{"order_id":"1"}` + "\n"} {
		res, err := e.s.Run(context.Background(), alice, "get_order", json.RawMessage(in), ChannelAPI, 0)
		if err != nil || string(res.Data) != `"1"` {
			t.Errorf("%q: %v %v", in, res, err)
		}
	}
	if called != 3 {
		t.Errorf("valid calls = %d", called)
	}
	// 空输入视为 {}
	sp := spec("ping", okCall(1))
	sp.InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
	e2 := newEnv(t, sp)
	for _, in := range []string{"", "null", "  "} {
		if _, err := e2.s.Run(context.Background(), alice, "ping", json.RawMessage(in), ChannelAPI, 0); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

// integer 的校验结果与 Go 标准库把同一 JSON 数字解码进 int64 的结果一致：校验通过的输入，接入方一定能解码。
func TestIntegerMatchesGoDecoding(t *testing.T) {
	sc, err := parseSchema(json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"0", "-0", "7", "-42", "9223372036854775807", "-9223372036854775808",
		"9223372036854775808", "3.0", "3.5", "1e3", "1E2", "-1.0e0", "0.0", "12345678901234567890"} {
		in := json.RawMessage(`{"n":` + v + `}`)
		var dst struct {
			N int64 `json:"n"`
		}
		goOK := json.Unmarshal(in, &dst) == nil
		ourOK := sc.validate(decodeObject(in)) == nil
		if goOK != ourOK {
			t.Errorf("%s: encoding/json int64 ok=%v, integer check ok=%v", v, goOK, ourOK)
		}
	}
}
