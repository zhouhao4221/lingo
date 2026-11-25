package connector

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// 限制。
const (
	DefaultTimeout = 10 * time.Second // Spec.Timeout 为 0 时的调用时限
	MaxTimeout     = 60 * time.Second // Spec.Timeout 的上限，超过时忽略该工具
	MaxNameLen     = 64
	MaxPerTool     = 8  // 每个工具同时在途的调用上限（含超时后仍未退出的）
	MaxInFlight    = 64 // 全部工具合计同时在途的调用上限
	MaxResultBytes = 8 << 10
	Retention      = 180 * 24 * time.Hour // 审计保留期
	leaseGrace     = 2 * time.Minute      // 待完成记录的租约 = 工具超时 + leaseGrace
)

// 注册被忽略的原因代码（Rejection.Reason）。
const (
	RejectInvalidName = "invalid_name"
	RejectDuplicate   = "duplicate_name" // 与先注册的有效工具同名（不区分大小写）
	RejectBuiltin     = "builtin_name"   // 与内置 MCP 工具同名（不区分大小写）
	RejectMode        = "write_mode"
	RejectEmptyScope  = "empty_scope"
	RejectScope       = "invalid_scope" // 含空白或控制字符（如首尾空格），按原值匹配时无人能调用
	RejectSchema      = "invalid_schema"
	RejectNilCall     = "nil_call"
	RejectTimeout     = "invalid_timeout"
)

// Caller 是调用人。Projects 是接入方给出的 User.Projects 原值（为空表示不限制），不是补全后的项目列表。
type Caller struct {
	ID       string
	Role     string
	Scopes   []string
	Projects []string
	User     any // 原样交回 Call
}

// Staff 报告调用人是否为 reviewer / admin。
func (c Caller) Staff() bool { return c.Role == "reviewer" || c.Role == "admin" }

func (c Caller) has(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func (c Caller) canAccess(project string) bool {
	if len(c.Projects) == 0 {
		return true
	}
	for _, p := range c.Projects {
		if p == project {
			return true
		}
	}
	return false
}

// Output 是 Call 的结果；Project 非空时声明所查数据的实际归属项目。
type Output struct {
	Project string
	Data    any
}

// CallFunc 是接入方的工具实现。
type CallFunc func(ctx context.Context, c Caller, input json.RawMessage) (Output, error)

// Spec 是一个待注册的工具。
type Spec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Scope       string
	Mode        string // 只接受 "read"
	Sensitive   []string
	Timeout     time.Duration
	Call        CallFunc
}

// Rejection 是一个被忽略的工具：Index 为其在注册列表中的序号。
type Rejection struct {
	Index  int
	Name   string
	Reason string
}

// Info 是对调用人可见的工具。
type Info struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type tool struct {
	Spec
	schema    *schema
	sensitive [][]string
	timeout   time.Duration
	br        breaker
	inflight  atomic.Int32
}

// declared 只保留 InputSchema 中声明的参数（审计不记录未声明字段的名称或值）。
func (t *tool) declared(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if _, ok := t.schema.props[k]; ok {
			out[k] = v
		}
	}
	return out
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// register 校验并接受工具：不合格的忽略；与内置工具或先注册的有效工具同名时忽略后者，不覆盖。
func register(specs []Spec, builtin []string) ([]*tool, []Rejection) {
	taken := map[string]bool{}
	for _, b := range builtin {
		taken[strings.ToLower(b)] = true
	}
	builtins := map[string]bool{}
	for k := range taken {
		builtins[k] = true
	}
	isBuiltin := func(n string) bool { return builtins[strings.ToLower(n)] }
	var out []*tool
	var rej []Rejection
	for i, sp := range specs {
		reason := ""
		var sc *schema
		var err error
		switch {
		case sp.Name == "" || len(sp.Name) > MaxNameLen || !nameRe.MatchString(sp.Name):
			reason = RejectInvalidName
		case isBuiltin(sp.Name):
			reason = RejectBuiltin
		case taken[strings.ToLower(sp.Name)]:
			reason = RejectDuplicate
		case sp.Mode != "read":
			reason = RejectMode
		case strings.TrimSpace(sp.Scope) == "":
			reason = RejectEmptyScope
		case strings.IndexFunc(sp.Scope, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
			reason = RejectScope
		case sp.Call == nil:
			reason = RejectNilCall
		case sp.Timeout < 0 || sp.Timeout > MaxTimeout:
			reason = RejectTimeout
		}
		if reason == "" {
			if sc, err = parseSchema(sp.InputSchema); err != nil {
				reason = RejectSchema
			}
		}
		if reason != "" {
			rej = append(rej, Rejection{Index: i, Name: safeName(sp.Name), Reason: reason})
			continue
		}
		taken[strings.ToLower(sp.Name)] = true
		t := &tool{Spec: sp, schema: sc, sensitive: parsePaths(sp.Sensitive), timeout: sp.Timeout}
		if t.timeout == 0 {
			t.timeout = DefaultTimeout
		}
		out = append(out, t)
	}
	return out, rej
}

// safeName 是写入日志的工具名：只保留名称允许的字符并截断，避免把接入方误放的内容带进日志。
func safeName(n string) string {
	var b strings.Builder
	for _, r := range n {
		if b.Len() >= MaxNameLen {
			break
		}
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}
