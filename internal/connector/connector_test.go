package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeAudit struct {
	mu         sync.Mutex
	recs       []Record
	leases     []time.Duration
	insertErr  error
	finishErr  error
	lastQuery  Query
	interrupts int
}

func (f *fakeAudit) InsertDataRequest(_ context.Context, r *Record, lease time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	r.ID = int64(len(f.recs) + 1)
	f.recs = append(f.recs, *r)
	f.leases = append(f.leases, lease)
	return nil
}

func (f *fakeAudit) FinishDataRequest(_ context.Context, r *Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finishErr != nil {
		return f.finishErr
	}
	f.recs[r.ID-1] = *r
	return nil
}

func (f *fakeAudit) ListDataRequests(_ context.Context, q Query) ([]Record, error) {
	f.lastQuery = q
	return nil, nil
}
func (f *fakeAudit) InterruptDataRequests(context.Context) (int, error) {
	f.interrupts++
	return 0, nil
}
func (f *fakeAudit) PurgeDataRequests(context.Context, time.Duration) (int, error) { return 0, nil }

func (f *fakeAudit) last() Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recs[len(f.recs)-1]
}

type sqlStateErr struct{}

func (sqlStateErr) Error() string    { return "duplicate key value (order_id)=(SECRET-1024)" }
func (sqlStateErr) SQLState() string { return "23505" }

const orderSchema = `{"type":"object","required":["order_id"],"properties":{"order_id":{"type":"string","description":"订单号"},"count":{"type":"integer"}}}`

func okCall(data any) CallFunc {
	return func(context.Context, Caller, json.RawMessage) (Output, error) { return Output{Data: data}, nil }
}

func spec(name string, call CallFunc) Spec {
	return Spec{Name: name, InputSchema: json.RawMessage(orderSchema), Scope: "order:read", Mode: "read", Call: call}
}

type env struct {
	s     *Service
	audit *fakeAudit
	logs  *bytes.Buffer
	now   time.Time
}

func newEnv(t *testing.T, specs ...Spec) *env {
	t.Helper()
	e := &env{audit: &fakeAudit{}, logs: &bytes.Buffer{}, now: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	var rej []Rejection
	e.s, rej = New(Options{Specs: specs, Builtin: []string{"ask"}, Projects: func() []string { return []string{"crm", "hr"} },
		Audit: e.audit, Logger: slog.New(slog.NewTextHandler(e.logs, nil)), Now: func() time.Time { return e.now }})
	if len(rej) > 0 {
		t.Fatalf("unexpected rejections: %v", rej)
	}
	return e
}

var alice = Caller{ID: "u1", Role: "external", Scopes: []string{"order:read"}, User: "alice"}

func TestRegister(t *testing.T) {
	call := okCall(nil)
	base := func(f func(*Spec)) Spec {
		s := spec("get_order", call)
		f(&s)
		return s
	}
	cases := []struct {
		name   string
		spec   Spec
		reason string
	}{
		{"empty name", base(func(s *Spec) { s.Name = "" }), RejectInvalidName},
		{"bad char", base(func(s *Spec) { s.Name = "get-order" }), RejectInvalidName},
		{"too long", base(func(s *Spec) { s.Name = strings.Repeat("a", 65) }), RejectInvalidName},
		{"builtin", base(func(s *Spec) { s.Name = "ASK" }), RejectBuiltin},
		{"write", base(func(s *Spec) { s.Mode = "write" }), RejectMode},
		{"empty scope", base(func(s *Spec) { s.Scope = " " }), RejectEmptyScope},
		{"nil call", base(func(s *Spec) { s.Call = nil }), RejectNilCall},
		{"negative timeout", base(func(s *Spec) { s.Timeout = -time.Second }), RejectTimeout},
		{"timeout over 60s", base(func(s *Spec) { s.Timeout = 61 * time.Second }), RejectTimeout},
		{"schema not object", base(func(s *Spec) { s.InputSchema = json.RawMessage(`[]`) }), RejectSchema},
		{"schema missing", base(func(s *Spec) { s.InputSchema = nil }), RejectSchema},
		{"schema invalid", base(func(s *Spec) { s.InputSchema = json.RawMessage(`{"properties":`) }), RejectSchema},
	}
	for _, c := range cases {
		ts, rej := register([]Spec{c.spec}, []string{"ask"})
		if len(ts) != 0 || len(rej) != 1 || rej[0].Reason != c.reason || rej[0].Index != 0 {
			t.Errorf("%s: tools=%d rej=%v, want %s", c.name, len(ts), rej, c.reason)
		}
	}
	// 60 秒恰好接受，0 取默认 10 秒；同名（不区分大小写）保留先注册的
	first := spec("get_order", call)
	first.Timeout = 60 * time.Second
	second := spec("GET_ORDER", call)
	dflt := spec("leave", call)
	ts, rej := register([]Spec{first, second, dflt}, []string{"ask"})
	if len(ts) != 2 || ts[0].Name != "get_order" || ts[0].timeout != 60*time.Second || ts[1].timeout != DefaultTimeout {
		t.Fatalf("tools = %+v", ts)
	}
	if len(rej) != 1 || rej[0].Index != 1 || rej[0].Reason != RejectDuplicate {
		t.Errorf("rej = %v", rej)
	}
	if n := safeName("a\nb secret"); n != "a?b?secret" {
		t.Errorf("safeName = %q", n)
	}
}

func TestPermissionAndInput(t *testing.T) {
	var got Caller
	e := newEnv(t, spec("get_order", func(_ context.Context, c Caller, in json.RawMessage) (Output, error) {
		got = c
		return Output{Data: map[string]any{"id": "1024"}}, nil
	}))
	if len(e.s.Visible(Caller{ID: "u2", Role: "external"})) != 0 || len(e.s.Visible(alice)) != 1 {
		t.Error("visibility by scope")
	}
	_, err1 := e.s.Run(context.Background(), Caller{ID: "u2", Role: "external"}, "get_order", json.RawMessage(`{"order_id":"1024"}`), ChannelAPI, 0)
	r1 := e.audit.last()
	_, err2 := e.s.Run(context.Background(), alice, "nope", json.RawMessage(`{"order_id":"1024"}`), ChannelAPI, 0)
	r2 := e.audit.last()
	if !errors.Is(err1, ErrNotFound) || !errors.Is(err2, ErrNotFound) || err1.Error() != err2.Error() {
		t.Errorf("forbidden / not found differ: %v / %v", err1, err2)
	}
	if r1.Status != StatusDenied || r1.Reason != ReasonForbidden || r1.Args != nil || r1.ArgNames[0] != "order_id" ||
		r2.Reason != ReasonNotFound || r2.Args != nil {
		t.Errorf("denied records = %+v / %+v", r1, r2)
	}
	for _, in := range []string{`{}`, `{"order_id":null}`, `{"order_id":1024}`, `{"order_id":"1","count":1.5}`, `[1]`} {
		_, err := e.s.Run(context.Background(), alice, "get_order", json.RawMessage(in), ChannelMCP, 0)
		var ie *InputError
		if !errors.As(err, &ie) || strings.Contains(err.Error(), "1024") || strings.Contains(err.Error(), "1.5") {
			t.Errorf("input %s: err = %v", in, err)
		}
		if r := e.audit.last(); r.Reason != ReasonInvalidInput || r.Args != nil {
			t.Errorf("input %s: record = %+v", in, r)
		}
	}
	var ie *InputError
	_, err := e.s.Run(context.Background(), alice, "get_order", nil, ChannelMCP, 0)
	if !errors.As(err, &ie) || ie.Param != "order_id" || ie.Want != "required" || ie.Desc != "订单号" {
		t.Errorf("missing required = %#v", err)
	}
	res, err := e.s.Run(context.Background(), alice, "get_order", json.RawMessage(`{"order_id":"1024"}`), ChannelAssistant, 7)
	if err != nil || string(res.Data) != `{"id":"1024"}` || got.User != "alice" || got.ID != "u1" {
		t.Fatalf("run = %+v, %v, caller %+v", res, err, got)
	}
	if r := e.audit.last(); r.Status != StatusOK || r.Args["order_id"] != "1024" || r.ConversationID != 7 || r.Channel != ChannelAssistant || r.Project != "" {
		t.Errorf("ok record = %+v", r)
	}
	if e.audit.leases[len(e.audit.leases)-1] != DefaultTimeout+leaseGrace {
		t.Errorf("lease = %v", e.audit.leases)
	}
}

func TestMaskAndTruncate(t *testing.T) {
	sp := spec("get_order", okCall(map[string]any{
		"phone":    "13800000000",
		"customer": map[string]any{"name": "张三", "phone": "13900000000"},
		"items":    []any{map[string]any{"sku": "a", "price": 10}, map[string]any{"sku": "b", "price": 20}},
		"rows":     []any{map[string]any{"phone": "1"}},
	}))
	sp.Sensitive = []string{"phone", "customer.phone", "items.*.price", "rows.phone", "order_id"}
	e := newEnv(t, sp)
	res, err := e.s.Run(context.Background(), alice, "get_order", json.RawMessage(`{"order_id":"SECRET"}`), ChannelAPI, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := string(res.Data)
	for _, leak := range []string{"13800000000", "13900000000", ":10", ":20", "\"1\""} {
		if strings.Contains(s, leak) {
			t.Errorf("not masked %s: %s", leak, s)
		}
	}
	if !strings.Contains(s, `"name":"张三"`) || !strings.Contains(s, `"sku":"a"`) {
		t.Errorf("over-masked: %s", s)
	}
	if r := e.audit.last(); r.Args["order_id"] != "***" {
		t.Errorf("args not masked: %v", r.Args)
	}

	big := strings.Repeat("汉", 5000) // 15000 字节
	data, text, truncated, err := encodeResult(map[string]string{"x": big}, nil)
	if err != nil || !truncated || data != nil || len(text) > MaxResultBytes || !utf8.ValidString(text) || len(text) < MaxResultBytes-3 {
		t.Errorf("truncate: %d bytes, truncated=%v, err=%v", len(text), truncated, err)
	}
	if _, _, truncated, _ := encodeResult(map[string]string{"x": "short"}, nil); truncated {
		t.Error("short result truncated")
	}
}

func TestAttribution(t *testing.T) {
	project := ""
	e := newEnv(t, spec("get_order", func(context.Context, Caller, json.RawMessage) (Output, error) {
		return Output{Project: project, Data: map[string]any{"secret": "PAYLOAD"}}, nil
	}))
	in := json.RawMessage(`{"order_id":"1024"}`)
	scoped := alice
	scoped.Projects = []string{"crm"}

	project = "crm"
	if _, err := e.s.Run(context.Background(), scoped, "get_order", in, ChannelAPI, 0); err != nil {
		t.Fatal(err)
	}
	if r := e.audit.last(); r.Project != "crm" || r.Status != StatusOK {
		t.Errorf("attributed = %+v", r)
	}
	// 不受限的调用人（Projects 为空）按现有语义可访问
	if _, err := e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0); err != nil {
		t.Errorf("unrestricted caller: %v", err)
	}
	var errs []error
	for _, p := range []string{"unknown_project", "hr"} { // 未配置 / 调用人无权访问
		project = p
		res, err := e.s.Run(context.Background(), scoped, "get_order", in, ChannelAPI, 0)
		if res != nil || !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: res=%v err=%v", p, res, err)
		}
		errs = append(errs, err)
		if r := e.audit.last(); r.Status != StatusDenied || r.Reason != ReasonForbidden || r.Project != "" || r.Args != nil {
			t.Errorf("%s: record = %+v", p, r)
		}
	}
	if errs[0].Error() != errs[1].Error() {
		t.Error("unknown and inaccessible projects differ")
	}
	// 归属拒绝不计入熔断
	for i := 0; i < 10; i++ {
		_, _ = e.s.Run(context.Background(), scoped, "get_order", in, ChannelAPI, 0)
	}
	project = ""
	if _, err := e.s.Run(context.Background(), scoped, "get_order", in, ChannelAPI, 0); err != nil {
		t.Errorf("breaker affected by project denials: %v", err)
	}
	if r := e.audit.last(); r.Project != "" {
		t.Errorf("undeclared project = %q", r.Project)
	}
}

func TestTimeoutAndConcurrency(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	sp := spec("slow", func(ctx context.Context, _ Caller, _ json.RawMessage) (Output, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release // 不响应 ctx
		return Output{Data: "late"}, nil
	})
	sp.Timeout = 30 * time.Millisecond
	e := newEnv(t, sp)
	in := json.RawMessage(`{"order_id":"1"}`)
	for i := 0; i < MaxPerTool; i++ {
		start := time.Now()
		_, err := e.s.Run(context.Background(), alice, "slow", in, ChannelAPI, 0)
		if !errors.Is(err, ErrFailed) || time.Since(start) > time.Second {
			t.Fatalf("call %d: err=%v after %v", i, err, time.Since(start))
		}
		if r := e.audit.last(); r.Status != StatusError || r.Reason != ReasonTimeout {
			t.Fatalf("record = %+v", r)
		}
		e.now = e.now.Add(10 * time.Minute) // 不让熔断干扰并发测试
	}
	// 8 个超时后仍未退出的调用占满名额：第 9 个不调用
	_, err := e.s.Run(context.Background(), alice, "slow", in, ChannelAPI, 0)
	mu.Lock()
	n := calls
	mu.Unlock()
	if !errors.Is(err, ErrUnavailable) || n != MaxPerTool {
		t.Fatalf("overloaded: err=%v calls=%d", err, n)
	}
	if r := e.audit.last(); r.Status != StatusUnavailable || r.Reason != ReasonOverloaded {
		t.Errorf("record = %+v", r)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for e.s.global.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.s.global.Load() != 0 {
		t.Fatal("slots not released after late return")
	}
	time.Sleep(20 * time.Millisecond)
	if !strings.Contains(e.logs.String(), "returned after timeout") || strings.Contains(e.logs.String(), "late") {
		t.Errorf("logs = %s", e.logs.String())
	}
}

func TestGlobalLimit(t *testing.T) {
	block := make(chan struct{})
	var specs []Spec
	for i := 0; i < 9; i++ {
		sp := spec("t"+string(rune('a'+i)), func(context.Context, Caller, json.RawMessage) (Output, error) { <-block; return Output{}, nil })
		sp.Timeout = 20 * time.Millisecond
		specs = append(specs, sp)
	}
	e := newEnv(t, specs...)
	defer close(block)
	in := json.RawMessage(`{"order_id":"1"}`)
	for i := 0; i < MaxInFlight; i++ {
		name := specs[i%8].Name
		e.now = e.now.Add(10 * time.Minute)
		if _, err := e.s.Run(context.Background(), alice, name, in, ChannelAPI, 0); !errors.Is(err, ErrFailed) {
			if errors.Is(err, ErrUnavailable) && i >= 8*MaxPerTool {
				break
			}
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := e.s.Run(context.Background(), alice, specs[8].Name, in, ChannelAPI, 0); !errors.Is(err, ErrUnavailable) {
		t.Errorf("global limit not enforced: %v", err)
	}
}

func TestBreaker(t *testing.T) {
	fail := true
	e := newEnv(t, spec("get_order", func(context.Context, Caller, json.RawMessage) (Output, error) {
		if fail {
			return Output{}, errors.New("upstream said SECRET-ERR for 13800000000")
		}
		return Output{Data: 1}, nil
	}))
	in := json.RawMessage(`{"order_id":"1"}`)
	run := func() error {
		_, err := e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0)
		return err
	}
	// 拒绝与参数错误不计失败
	for i := 0; i < 10; i++ {
		_, _ = e.s.Run(context.Background(), alice, "get_order", json.RawMessage(`{}`), ChannelAPI, 0)
		_, _ = e.s.Run(context.Background(), Caller{ID: "x", Role: "external"}, "get_order", in, ChannelAPI, 0)
	}
	for i := 0; i < 4; i++ {
		if err := run(); !errors.Is(err, ErrFailed) {
			t.Fatalf("fail %d: %v", i, err)
		}
	}
	// 超过 5 分钟窗口：重新计数
	e.now = e.now.Add(6 * time.Minute)
	for i := 0; i < 4; i++ {
		_ = run()
	}
	if err := run(); !errors.Is(err, ErrFailed) { // 第 5 次连续失败（窗口内）：本次照常失败，之后暂停
		t.Fatalf("5th: %v", err)
	}
	if err := run(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("open: %v", err)
	}
	if r := e.audit.last(); r.Reason != ReasonCircuitOpen {
		t.Errorf("record = %+v", r)
	}
	e.now = e.now.Add(61 * time.Second)
	if err := run(); !errors.Is(err, ErrFailed) { // 试探失败：再暂停
		t.Fatalf("probe: %v", err)
	}
	if err := run(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reopened: %v", err)
	}
	e.now = e.now.Add(61 * time.Second)
	fail = false
	if err := run(); err != nil {
		t.Fatalf("probe success: %v", err)
	}
	if err := run(); err != nil {
		t.Fatalf("closed: %v", err)
	}
	for _, r := range e.audit.recs {
		if r.Reason == "SECRET" || strings.Contains(r.Reason, "SECRET") {
			t.Errorf("error text in audit: %+v", r)
		}
	}
	if l := e.logs.String(); strings.Contains(l, "SECRET") || strings.Contains(l, "13800000000") || !strings.Contains(l, "circuit opened") {
		t.Errorf("logs = %s", l)
	}
}

func TestPanicAndAuditFailures(t *testing.T) {
	called := 0
	e := newEnv(t,
		spec("boom", func(context.Context, Caller, json.RawMessage) (Output, error) { panic("SECRET-PANIC 13800000000") }),
		spec("get_order", func(context.Context, Caller, json.RawMessage) (Output, error) {
			called++
			return Output{Data: "ok"}, nil
		}))
	in := json.RawMessage(`{"order_id":"SECRET-ARG"}`)
	if _, err := e.s.Run(context.Background(), alice, "boom", in, ChannelAPI, 0); !errors.Is(err, ErrFailed) {
		t.Errorf("panic: %v", err)
	}
	if r := e.audit.last(); r.Reason != ReasonPanic {
		t.Errorf("panic record = %+v", r)
	}
	// 待完成记录写不进：不调用
	e.audit.insertErr = sqlStateErr{}
	if _, err := e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0); !errors.Is(err, ErrUnavailable) || called != 0 {
		t.Errorf("insert failure: err=%v called=%d", err, called)
	}
	// 更新失败：结果照常返回
	e.audit.insertErr, e.audit.finishErr = nil, sqlStateErr{}
	res, err := e.s.Run(context.Background(), alice, "get_order", in, ChannelAPI, 0)
	if err != nil || string(res.Data) != `"ok"` || called != 1 {
		t.Errorf("finish failure: %v %v", res, err)
	}
	if r := e.audit.last(); r.Status != StatusPending {
		t.Errorf("record should stay pending: %+v", r)
	}
	l := e.logs.String()
	if strings.Contains(l, "SECRET") || strings.Contains(l, "13800000000") || !strings.Contains(l, "23505") {
		t.Errorf("logs leak or miss code: %s", l)
	}
}

func TestCanceled(t *testing.T) {
	e := newEnv(t, spec("get_order", func(ctx context.Context, _ Caller, _ json.RawMessage) (Output, error) {
		<-ctx.Done()
		return Output{}, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := e.s.Run(ctx, alice, "get_order", json.RawMessage(`{"order_id":"1"}`), ChannelAPI, 0); !errors.Is(err, ErrFailed) {
		t.Errorf("err = %v", err)
	}
	if r := e.audit.last(); r.Reason != ReasonCanceled || r.Status != StatusError {
		t.Errorf("record = %+v", r)
	}
}

func TestListScope(t *testing.T) {
	e := newEnv(t, spec("get_order", okCall(1)))
	for _, c := range []struct {
		caller Caller
		want   []string
	}{
		{Caller{ID: "r1", Role: "reviewer", Projects: []string{"crm"}}, []string{"crm"}},
		{Caller{ID: "a1", Role: "admin"}, nil}, // Projects 为空不授予跨用户审计
		{Caller{ID: "u1", Role: "external", Projects: []string{"crm"}}, nil},
		{Caller{ID: "g1", Role: "agent", Projects: []string{"crm"}}, nil},
	} {
		if _, err := e.s.List(context.Background(), c.caller, Filter{UserID: "other"}); err != nil {
			t.Fatal(err)
		}
		q := e.audit.lastQuery
		if q.ViewerID != c.caller.ID || len(q.Projects) != len(c.want) || (len(c.want) > 0 && q.Projects[0] != c.want[0]) || q.Limit != 50 || q.UserID != "other" {
			t.Errorf("%s: query = %+v", c.caller.ID, q)
		}
	}
	if _, err := e.s.List(context.Background(), alice, Filter{Status: "bogus"}); !errors.Is(err, ErrBadQuery) {
		t.Errorf("bad status: %v", err)
	}
	if _, err := e.s.List(context.Background(), alice, Filter{Limit: 999}); err != nil || e.audit.lastQuery.Limit != 200 {
		t.Errorf("limit cap: %v %d", err, e.audit.lastQuery.Limit)
	}
	if err := e.s.Maintain(context.Background()); err != nil || e.audit.interrupts != 1 {
		t.Errorf("maintain: %v", err)
	}
}
