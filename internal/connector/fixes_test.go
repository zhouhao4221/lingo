package connector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// 交付前修订项的回归测试。

type panicMarshal struct{}

func (panicMarshal) MarshalJSON() ([]byte, error) { panic("SECRET-IN-PANIC 13800000000") }

type errMarshal struct{}

func (errMarshal) MarshalJSON() ([]byte, error) {
	return nil, errors.New("SECRET-IN-ERROR 13800000000")
}

func TestEncodeFailureClosesAuditAndBreaker(t *testing.T) {
	var data any
	sp := spec("get_order", func(context.Context, Caller, json.RawMessage) (Output, error) { return Output{Data: data}, nil })
	sp.Sensitive = []string{"phone"}
	e := newEnv(t, sp)
	in := json.RawMessage(`{"order_id":"1"}`)
	for i, d := range []any{panicMarshal{}, errMarshal{}, map[string]any{"phone": panicMarshal{}}} {
		data = d
		res, err := e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0)
		if res != nil || !errors.Is(err, ErrFailed) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("%d: res=%v err=%v", i, res, err)
		}
		if r := e.audit.last(); r.Status != StatusError || r.Reason != ReasonEncode {
			t.Errorf("%d: record = %+v", i, r)
		}
	}
	if e.s.global.Load() != 0 {
		t.Error("slot leaked")
	}
	// 编码失败计入熔断：再失败两次（共 5 次）后暂停
	for i := 0; i < 2; i++ {
		_, _ = e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0)
	}
	if _, err := e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0); !errors.Is(err, ErrUnavailable) {
		t.Errorf("breaker not opened by encode failures: %v", err)
	}
	if l := e.logs.String(); strings.Contains(l, "SECRET") || strings.Contains(l, "13800000000") {
		t.Errorf("logs leak: %s", l)
	}
}

func TestAuditKeepsDeclaredParamsOnly(t *testing.T) {
	e := newEnv(t, spec("get_order", okCall(1)))
	in := json.RawMessage(`{"order_id":"1024","undeclared_note":"PRIVATE-TEXT","PRIVATE-KEY-NAME":1}`)
	if _, err := e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0); err != nil {
		t.Fatal(err)
	}
	check := func(label string, r Record, wantNames int) {
		t.Helper()
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "PRIVATE") || strings.Contains(string(b), "undeclared") || len(r.ArgNames) != wantNames {
			t.Errorf("%s: record = %s", label, b)
		}
	}
	check("ok", e.audit.last(), 1)
	_, _ = e.s.Run(context.Background(), Caller{ID: "x", Role: "external"}, "get_order", in, ChannelAPI, 0)
	check("forbidden", e.audit.last(), 1)
	_, _ = e.s.Run(context.Background(), alice, "no_such_tool", in, ChannelAPI, 0)
	check("not found", e.audit.last(), 0)
	_, _ = e.s.Run(context.Background(), alice, "get_order", json.RawMessage(`{"undeclared_note":"PRIVATE-TEXT"}`), ChannelAPI, 0)
	check("invalid input", e.audit.last(), 0)
}

func TestBreakerIgnoresStaleCalls(t *testing.T) {
	var b breaker
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	stale, ok := b.allow(now) // 暂停前就在途的调用
	if !ok {
		t.Fatal("closed breaker rejected")
	}
	for i := 0; i < failThreshold; i++ {
		tk, _ := b.allow(now)
		b.failure(tk, now)
	}
	if _, ok := b.allow(now); ok {
		t.Fatal("not opened")
	}
	b.success(stale) // 暂停期间旧调用晚到的成功：不得关闭
	if _, ok := b.allow(now.Add(30 * time.Second)); ok {
		t.Error("stale success closed the breaker")
	}
	probe, ok := b.allow(now.Add(61 * time.Second))
	if !ok || !probe.probe {
		t.Fatal("no probe")
	}
	b.failure(stale, now.Add(62*time.Second)) // 旧调用晚到的失败：不得干扰试探
	b.success(stale)
	if !b.probing || b.openUntil.IsZero() {
		t.Error("stale result changed probe state")
	}
	b.success(probe)
	if tk, ok := b.allow(now.Add(63 * time.Second)); !ok || tk.probe {
		t.Error("probe success did not close")
	}
	// 试探失败后再暂停：之前的试探凭据也失效
	for i := 0; i < failThreshold; i++ {
		tk, _ := b.allow(now.Add(70 * time.Second))
		b.failure(tk, now.Add(70*time.Second))
	}
	p2, _ := b.allow(now.Add(140 * time.Second))
	b.failure(p2, now.Add(140*time.Second))
	b.success(p2) // 同一凭据在重开后不再生效
	if _, ok := b.allow(now.Add(150 * time.Second)); ok {
		t.Error("old probe ticket reopened the breaker")
	}
}

func TestBreakerStaleInFlightService(t *testing.T) {
	release := make(chan struct{})
	slow := true
	sp := spec("get_order", func(ctx context.Context, _ Caller, _ json.RawMessage) (Output, error) {
		if slow {
			<-release
			return Output{Data: "late"}, nil
		}
		return Output{}, errors.New("down")
	})
	sp.Timeout = 20 * time.Millisecond
	e := newEnv(t, sp)
	in := json.RawMessage(`{"order_id":"1"}`)
	// 5 次超时打开熔断；这些调用仍在途
	for i := 0; i < failThreshold; i++ {
		_, _ = e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0)
	}
	close(release) // 旧调用晚到返回（结果已丢弃，不计入熔断）
	time.Sleep(30 * time.Millisecond)
	if _, err := e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0); !errors.Is(err, ErrUnavailable) {
		t.Errorf("late returns closed the breaker: %v", err)
	}
}

func TestTimeoutBoundary(t *testing.T) {
	// Call 在时限到达后才返回成功：无论调度先后都按超时处理，结果丢弃
	sp := spec("edge", func(ctx context.Context, _ Caller, _ json.RawMessage) (Output, error) {
		<-ctx.Done()
		return Output{Data: "too late"}, nil
	})
	sp.Timeout = 5 * time.Millisecond
	e := newEnv(t, sp)
	for i := 0; i < 30; i++ {
		e.now = e.now.Add(10 * time.Minute)
		res, err := e.s.Run(context.Background(), alice, "edge", json.RawMessage(`{"order_id":"1"}`), ChannelAPI, 0)
		if res != nil || !errors.Is(err, ErrFailed) || e.audit.last().Reason != ReasonTimeout {
			t.Fatalf("%d: res=%v err=%v rec=%+v", i, res, err, e.audit.last())
		}
	}
	// 结果大小边界：恰好 8KB 不截断，多 1 字节截断
	exact := strings.Repeat("a", MaxResultBytes-2) // JSON 字符串加两个引号
	if _, _, tr, _ := encodeResult(exact, nil); tr {
		t.Error("exactly 8KB truncated")
	}
	if _, _, tr, _ := encodeResult(exact+"a", nil); !tr {
		t.Error("8KB+1 not truncated")
	}
	// 注册边界：60 秒接受，60 秒 + 1 纳秒拒绝
	if ts, _ := register([]Spec{func() Spec { s := spec("a", okCall(1)); s.Timeout = MaxTimeout + 1; return s }()}, nil); len(ts) != 0 {
		t.Error("timeout just over 60s accepted")
	}
}
