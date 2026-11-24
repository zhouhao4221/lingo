package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// 错误：都不含参数值、原始结果或 Call 的错误原文。
var (
	// ErrNotFound 表示工具不存在、调用人无权调用，或结果声明的归属项目不可访问（三者不区分）。
	ErrNotFound = errors.New("connector: not found")
	// ErrUnavailable 表示暂时不可用：熔断中、在途调用已满，或审计写入失败。
	ErrUnavailable = errors.New("connector: temporarily unavailable")
	// ErrFailed 表示调用失败：Call 返回错误、超时、panic 或结果无法编码。
	ErrFailed = errors.New("connector: call failed")
	// ErrBadQuery 表示审计查询条件不合法。
	ErrBadQuery = errors.New("connector: invalid query")
)

// 入口（Record.Channel）。
const (
	ChannelAssistant = "assistant"
	ChannelMCP       = "mcp"
	ChannelAPI       = "api"
)

// 审计状态与原因。
const (
	StatusPending     = "pending"
	StatusOK          = "ok"
	StatusDenied      = "denied"
	StatusError       = "error"
	StatusUnavailable = "unavailable"
	StatusInterrupted = "interrupted"

	ReasonNotFound     = "not_found"
	ReasonForbidden    = "forbidden" // 无工具权限，或结果声明的归属项目不可访问
	ReasonInvalidInput = "invalid_input"
	ReasonTimeout      = "timeout"
	ReasonCallError    = "call_error"
	ReasonPanic        = "panic"
	ReasonEncode       = "encode_error"
	ReasonCanceled     = "canceled"
	ReasonCircuitOpen  = "circuit_open"
	ReasonOverloaded   = "overloaded"
	ReasonInterrupted  = "interrupted"
)

// Record 是一条审计记录，不含结果与错误原文。ArgNames / Args 只含 InputSchema 声明的参数；Args 为 nil 表示不记参数值
// （拒绝记录）。
type Record struct {
	ID             int64
	UserID         string
	UserRole       string
	Project        string // 归属项目：只取 Output.Project 的声明
	Tool           string
	Channel        string
	ConversationID int64
	ArgNames       []string
	Args           map[string]string
	Status         string
	Reason         string
	Duration       time.Duration
	Truncated      bool
	CreatedAt      time.Time
	FinishedAt     *time.Time
}

// Query 是审计查询：返回 ViewerID 本人的记录，以及归属项目在 Projects 中的记录（Projects 为空时不返回他人的记录）。
type Query struct {
	ViewerID string
	Projects []string
	Tool     string
	UserID   string
	Status   string
	Since    time.Time
	Until    time.Time
	BeforeID int64
	Limit    int
}

// AuditStore 是审计仓储，由 internal/store 实现。时间一律取数据库时间。
type AuditStore interface {
	// InsertDataRequest 写入记录并回填 ID 与 CreatedAt。Status 为 pending 时租约到期时间为数据库当前时间 + lease。
	InsertDataRequest(ctx context.Context, r *Record, lease time.Duration) error
	// FinishDataRequest 把仍为 pending 的记录更新为最终状态（已被标为中断的不再更新）。
	FinishDataRequest(ctx context.Context, r *Record) error
	ListDataRequests(ctx context.Context, q Query) ([]Record, error)
	// InterruptDataRequests 把租约已过期的 pending 记录标为中断（单条语句的条件更新，并发时每条只标一次）。
	InterruptDataRequests(ctx context.Context) (int, error)
	PurgeDataRequests(ctx context.Context, before time.Time) (int, error)
}

// Options 是 Service 的配置。
type Options struct {
	Specs    []Spec
	Builtin  []string        // 内置 MCP 工具名
	Projects func() []string // 已配置的项目 key
	Audit    AuditStore
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service 是业务数据工具的调用入口。
type Service struct {
	o      Options
	tools  []*tool
	byName map[string]*tool
	global atomic.Int32
}

// New 校验并接受工具，返回被忽略的工具（由调用方记 Warn）。
func New(o Options) (*Service, []Rejection) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Projects == nil {
		o.Projects = func() []string { return nil }
	}
	ts, rej := register(o.Specs, o.Builtin)
	s := &Service{o: o, tools: ts, byName: map[string]*tool{}}
	for _, t := range ts {
		s.byName[t.Name] = t
	}
	return s, rej
}

// Empty 报告没有可用的工具。
func (s *Service) Empty() bool { return len(s.tools) == 0 }

// All 返回全部已接受的工具（按注册顺序）。
func (s *Service) All() []Info {
	out := make([]Info, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, Info{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// Visible 返回调用人可调用的工具（按注册顺序）。
func (s *Service) Visible(c Caller) []Info {
	var out []Info
	for _, t := range s.tools {
		if c.has(t.Scope) {
			out = append(out, Info{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	return out
}

// CanCall 报告调用人能否调用 name。
func (s *Service) CanCall(c Caller, name string) bool {
	t := s.byName[name]
	return t != nil && c.has(t.Scope)
}

// Result 是一次成功调用的结果：未截断时 Data 为脱敏后的 JSON，截断时 Text 为截断后的 JSON 文本。
type Result struct {
	Tool        string
	Description string
	Data        json.RawMessage
	Text        string
	Truncated   bool
	Project     string
}

// Run 以调用人身份调用工具。convID 为问答入口的会话（其他入口为 0）。
func (s *Service) Run(ctx context.Context, c Caller, name string, input json.RawMessage, channel string, convID int64) (*Result, error) {
	start := time.Now()
	rec := &Record{UserID: c.ID, UserRole: c.Role, Tool: clip(name, MaxNameLen), Channel: channel, ConversationID: convID}
	args := decodeObject(input)
	t := s.byName[name]
	if t == nil || !c.has(t.Scope) {
		// 审计只记 InputSchema 声明的参数名：工具不存在时没有可信的声明，不记参数
		reason, names := ReasonNotFound, map[string]any(nil)
		if t != nil {
			reason, names = ReasonForbidden, t.declared(args)
		}
		s.record(ctx, rec, names, StatusDenied, reason, start, false)
		return nil, ErrNotFound
	}
	if err := t.schema.validate(args); err != nil {
		s.record(ctx, rec, t.declared(args), StatusDenied, ReasonInvalidInput, start, false)
		return nil, err
	}
	masked := t.declared(mask(decodeObject(input), t.sensitive).(map[string]any))
	tk, ok := t.br.allow(s.o.Now())
	if !ok {
		s.record(ctx, rec, masked, StatusUnavailable, ReasonCircuitOpen, start, true)
		return nil, ErrUnavailable
	}
	if !s.acquire(t) {
		t.br.neutral(tk)
		s.record(ctx, rec, masked, StatusUnavailable, ReasonOverloaded, start, true)
		return nil, ErrUnavailable
	}
	rec.ArgNames, rec.Args = summarize(masked)
	rec.Status = StatusPending
	if err := s.o.Audit.InsertDataRequest(ctx, rec, t.timeout+leaseGrace); err != nil {
		s.release(t)
		t.br.neutral(tk)
		s.o.Logger.Error("lingo: data tool audit failed, call skipped", "tool", t.Name, "code", dbCode(err))
		return nil, ErrUnavailable
	}
	out, reason := s.call(ctx, t, c, normalize(input))
	var res *Result
	var err error
	switch {
	case reason == ReasonCanceled:
		t.br.neutral(tk)
		err = ErrFailed
	case reason != "":
		s.fail(t, tk)
		err = ErrFailed
	case out.Project != "" && (!s.configured(out.Project) || !c.canAccess(out.Project)):
		// 声明的归属项目不存在或不可访问：丢弃结果，与工具不存在相同；记录不带归属与参数值
		t.br.neutral(tk)
		reason, rec.Args = ReasonForbidden, nil
		s.finish(ctx, rec, StatusDenied, reason, start)
		return nil, ErrNotFound
	default:
		data, text, truncated, encErr := safeEncode(out.Data, t.sensitive)
		if encErr != nil {
			// 编码或脱敏失败（含 panic）：结果丢弃，按失败收尾审计与熔断；错误内容可能含业务数据，不记录
			reason = ReasonEncode
			s.fail(t, tk)
			err = ErrFailed
			break
		}
		t.br.success(tk)
		rec.Project, rec.Truncated = out.Project, truncated
		res = &Result{Tool: t.Name, Description: t.Description, Data: data, Text: text, Truncated: truncated, Project: out.Project}
	}
	if err != nil {
		s.finish(ctx, rec, StatusError, reason, start)
		return nil, err
	}
	s.finish(ctx, rec, StatusOK, "", start)
	return res, nil
}

func (s *Service) fail(t *tool, tk ticket) {
	if t.br.failure(tk, s.o.Now()) {
		s.o.Logger.Warn("lingo: data tool circuit opened", "tool", t.Name, "pause", openFor.String())
	}
}

func (s *Service) configured(project string) bool {
	for _, p := range s.o.Projects() {
		if p == project {
			return true
		}
	}
	return false
}

func (s *Service) acquire(t *tool) bool {
	if s.global.Add(1) > MaxInFlight {
		s.global.Add(-1)
		return false
	}
	if t.inflight.Add(1) > MaxPerTool {
		t.inflight.Add(-1)
		s.global.Add(-1)
		return false
	}
	return true
}

func (s *Service) release(t *tool) {
	t.inflight.Add(-1)
	s.global.Add(-1)
}

// call 在超时内执行 Call，返回失败原因（成功为空）。Call 在独立 goroutine 中执行并一直占用并发名额到真正返回；
// 超时后返回的结果直接丢弃。
func (s *Service) call(ctx context.Context, t *tool, c Caller, input json.RawMessage) (Output, string) {
	cctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	type result struct {
		out      Output
		err      error
		panicked bool
	}
	done := make(chan result, 1)
	var late atomic.Bool
	started := time.Now()
	go func() {
		defer s.release(t)
		defer func() {
			if r := recover(); r != nil {
				done <- result{panicked: true}
			}
			if late.Load() {
				s.o.Logger.Warn("lingo: data tool returned after timeout, result discarded", "tool", t.Name,
					"elapsed", time.Since(started).Round(time.Millisecond).String())
			}
		}()
		out, err := t.Call(cctx, c, input)
		done <- result{out: out, err: err}
	}()
	select {
	case r := <-done:
		// 超时边界：结果在时限到达之后才取到时按超时处理并丢弃，与 Done 分支一致（不因调度先后而不同）
		if ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return Output{}, ReasonTimeout
		}
		switch {
		case r.panicked:
			s.o.Logger.Error("lingo: data tool panicked", "tool", t.Name)
			return Output{}, ReasonPanic
		case r.err == nil:
			return r.out, ""
		case ctx.Err() != nil:
			return Output{}, ReasonCanceled
		case cctx.Err() != nil:
			return Output{}, ReasonTimeout
		}
		return Output{}, ReasonCallError
	case <-cctx.Done():
		late.Store(true)
		if ctx.Err() != nil {
			return Output{}, ReasonCanceled
		}
		return Output{}, ReasonTimeout
	}
}

// record 直接写入一条最终状态的记录（不调用 Call 的情形）。withValues 为假时只记参数名。
func (s *Service) record(ctx context.Context, rec *Record, args map[string]any, status, reason string, start time.Time, withValues bool) {
	rec.ArgNames, rec.Args = summarize(args)
	if !withValues {
		rec.Args = nil
	}
	rec.Status, rec.Reason, rec.Duration = status, reason, time.Since(start)
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.o.Audit.InsertDataRequest(actx, rec, 0); err != nil {
		s.o.Logger.Error("lingo: data tool audit failed", "status", status, "reason", reason, "code", dbCode(err))
	}
}

// finish 更新待完成记录；失败只记日志，不影响本次结果（记录保持待完成，租约过期后由 Maintain 标为中断）。
func (s *Service) finish(ctx context.Context, rec *Record, status, reason string, start time.Time) {
	rec.Status, rec.Reason, rec.Duration = status, reason, time.Since(start)
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.o.Audit.FinishDataRequest(actx, rec); err != nil {
		s.o.Logger.Error("lingo: data tool audit update failed", "tool", rec.Tool, "audit_id", rec.ID, "code", dbCode(err))
	}
}

// Maintain 把租约过期的待完成记录标为中断，并清理超过保留期的记录。
func (s *Service) Maintain(ctx context.Context) error {
	if _, err := s.o.Audit.InterruptDataRequests(ctx); err != nil {
		return fmt.Errorf("interrupt data requests: %s", dbCode(err))
	}
	if _, err := s.o.Audit.PurgeDataRequests(ctx, s.o.Now().Add(-Retention)); err != nil {
		return fmt.Errorf("purge data requests: %s", dbCode(err))
	}
	return nil
}

// Filter 是审计查询条件。
type Filter struct {
	Tool     string
	UserID   string
	Status   string
	Since    time.Time
	Until    time.Time
	BeforeID int64
	Limit    int
}

var statuses = []string{StatusPending, StatusOK, StatusDenied, StatusError, StatusUnavailable, StatusInterrupted}

// List 返回 viewer 可见的审计记录（新的在前）：本人的，以及 reviewer / admin 在其明确列出的项目内的；
// User.Projects 为空不授予跨用户审计。limit 为 0 时默认 50，最多 200。
func (s *Service) List(ctx context.Context, viewer Caller, f Filter) ([]Record, error) {
	if f.Status != "" {
		ok := false
		for _, st := range statuses {
			ok = ok || st == f.Status
		}
		if !ok {
			return nil, fmt.Errorf("%w: unknown status", ErrBadQuery)
		}
	}
	if f.Limit < 0 || f.BeforeID < 0 {
		return nil, fmt.Errorf("%w: limit and before must not be negative", ErrBadQuery)
	}
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	q := Query{ViewerID: viewer.ID, Tool: f.Tool, UserID: f.UserID, Status: f.Status, Since: f.Since, Until: f.Until,
		BeforeID: f.BeforeID, Limit: f.Limit}
	if viewer.Staff() {
		q.Projects = viewer.Projects
	}
	return s.o.Audit.ListDataRequests(ctx, q)
}

// dbCode 返回数据库错误码（SQLSTATE），不返回错误详情（其中可能含参数值）。
func dbCode(err error) string {
	var se interface{ SQLState() string }
	if errors.As(err, &se) {
		return se.SQLState()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "unknown"
}
