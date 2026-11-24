package lingo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zhouhao4221/lingo/internal/connector"
)

// 业务数据工具（REQ-008）的错误。错误信息不含参数值、结果或 Call 返回的错误原文。
var (
	// ErrToolUnavailable 表示工具暂时不可用：熔断中（5 分钟内连续失败 5 次后暂停 1 分钟）、在途调用已满
	// （每个工具 8 个、合计 64 个），或审计记录写入失败（写不进审计时不调用）。
	ErrToolUnavailable = errors.New("lingo: data tool temporarily unavailable")
	// ErrToolFailed 表示调用失败：Call 返回错误、超时、panic 或结果无法编码为 JSON。
	ErrToolFailed = errors.New("lingo: data tool call failed")
)

// ToolResult 是一次业务数据工具调用的结果，已按 Tool.Sensitive 脱敏。结果超过 8KB 时 Truncated 为真，
// Data 为空，Text 为截断后的 JSON 文本。
type ToolResult struct {
	Tool      string          `json:"tool"`
	Data      json.RawMessage `json:"data,omitempty"`
	Text      string          `json:"text,omitempty"`
	Truncated bool            `json:"truncated"`
}

// CallTool 以 u 的身份调用业务数据工具 name（input 为参数 JSON 对象，可为空）。规则同问答与 MCP：校验 u.Scopes、
// 参数（只含参数名的错误信息）、超时、并发上限与熔断，调用前写审计。
// 工具不存在、u 无权调用、或结果声明的归属项目不可访问时返回 ErrNotFound（不区分）；参数不合法返回 ErrInvalidInput；
// 另见 ErrToolUnavailable、ErrToolFailed。
func (l *Lingo) CallTool(ctx context.Context, u User, name string, input json.RawMessage) (ToolResult, error) {
	c, err := dataCaller(u)
	if err != nil {
		return ToolResult{}, err
	}
	r, err := l.data.Run(ctx, c, name, input, connector.ChannelAPI, 0)
	if err != nil {
		return ToolResult{}, dataErr(err)
	}
	return ToolResult{Tool: r.Tool, Data: r.Data, Text: r.Text, Truncated: r.Truncated}, nil
}

// DataRequest 是一次业务数据工具调用的审计摘要，不含结果与错误原文。
type DataRequest struct {
	ID       int64  `json:"id"`
	UserID   string `json:"user_id"`
	UserRole string `json:"user_role"`
	// Project 是工具结果声明的归属项目（ToolOutput.Project），未声明为空。
	Project string `json:"project,omitempty"`
	Tool    string `json:"tool"`
	// Channel 是入口：assistant（知识问答）| mcp | api（CallTool）。
	Channel        string `json:"channel"`
	ConversationID int64  `json:"conversation_id,omitempty"`
	// ArgNames 是参数名；Args 是参数名到截断（32 字）、脱敏后的值，拒绝记录没有 Args。
	ArgNames []string          `json:"arg_names"`
	Args     map[string]string `json:"args,omitempty"`
	// Status 是 pending | ok | denied | error | unavailable | interrupted；Reason 是原因代码，如 not_found、forbidden、
	// invalid_input、timeout、call_error、panic、canceled、circuit_open、overloaded、interrupted。
	Status     string     `json:"status"`
	Reason     string     `json:"reason,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	Truncated  bool       `json:"truncated"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// DataRequestFilter 是 ListDataRequests 的条件；Before 为上一页最后一条的 ID，Limit 为 0 时默认 50，最多 200。
type DataRequestFilter struct {
	Tool   string
	UserID string
	Status string
	Since  time.Time
	Until  time.Time
	Before int64
	Limit  int
}

// ListDataRequests 返回 u 可见的业务数据调用审计，新的在前：每个人都能看到本人的记录；reviewer / admin 另可看到归属
// 项目在 u.Projects 中明确列出的记录。u.Projects 为空不授予跨用户审计（这一点与其他功能中"为空表示不限制"不同），
// 归属为空的记录只有调用人本人可见。按用户筛选范围外的人员时返回空列表。
func (l *Lingo) ListDataRequests(ctx context.Context, u User, f DataRequestFilter) ([]DataRequest, error) {
	c, err := dataCaller(u)
	if err != nil {
		return nil, err
	}
	rs, err := l.data.List(ctx, c, connector.Filter{Tool: f.Tool, UserID: f.UserID, Status: f.Status, Since: f.Since,
		Until: f.Until, BeforeID: f.Before, Limit: f.Limit})
	if err != nil {
		return nil, dataErr(err)
	}
	out := make([]DataRequest, 0, len(rs))
	for _, r := range rs {
		names := r.ArgNames
		if names == nil {
			names = []string{}
		}
		out = append(out, DataRequest{ID: r.ID, UserID: r.UserID, UserRole: r.UserRole, Project: r.Project, Tool: r.Tool,
			Channel: r.Channel, ConversationID: r.ConversationID, ArgNames: names, Args: r.Args, Status: r.Status,
			Reason: r.Reason, DurationMS: r.Duration.Milliseconds(), Truncated: r.Truncated, CreatedAt: r.CreatedAt,
			FinishedAt: r.FinishedAt})
	}
	return out, nil
}

// dataCaller 转换调用人；Projects 保留 User.Projects 原值（审计可见范围只认明确列出的项目）。
func dataCaller(u User) (connector.Caller, error) {
	switch {
	case u.ID == "":
		return connector.Caller{}, fmt.Errorf("%w: caller has no user id", ErrForbidden)
	case u.Role != RoleExternal && u.Role != RoleAgent && u.Role != RoleReviewer && u.Role != RoleAdmin:
		return connector.Caller{}, fmt.Errorf("%w: unknown role", ErrForbidden)
	}
	return connector.Caller{ID: u.ID, Role: string(u.Role), Scopes: u.Scopes, Projects: u.Projects, User: u}, nil
}

func dataErr(err error) error {
	var ie *connector.InputError
	switch {
	case errors.As(err, &ie):
		return fmt.Errorf("%w: %s", ErrInvalidInput, ie.Error())
	case errors.Is(err, connector.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, connector.ErrUnavailable):
		return ErrToolUnavailable
	case errors.Is(err, connector.ErrFailed):
		return ErrToolFailed
	case errors.Is(err, connector.ErrBadQuery):
		return fmt.Errorf("%w%s", ErrInvalidInput, err.Error()[len(connector.ErrBadQuery.Error()):])
	}
	return err
}

// newDataTools 校验 Config.Tools：被忽略的工具记 Warn（只含序号、名称与原因代码）。
func (l *Lingo) newDataTools(builtin []string) *connector.Service {
	specs := make([]connector.Spec, 0, len(l.cfg.Tools))
	for _, t := range l.cfg.Tools {
		sp := connector.Spec{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, Scope: t.Scope,
			Mode: string(t.Mode), Sensitive: t.Sensitive, Timeout: t.Timeout}
		if call := t.Call; call != nil {
			sp.Call = func(ctx context.Context, c connector.Caller, input json.RawMessage) (connector.Output, error) {
				v, err := call(ctx, c.User.(User), input)
				if err != nil {
					return connector.Output{}, err
				}
				switch o := v.(type) {
				case ToolOutput:
					return connector.Output{Project: o.Project, Data: o.Data}, nil
				case *ToolOutput:
					if o != nil {
						return connector.Output{Project: o.Project, Data: o.Data}, nil
					}
				}
				return connector.Output{Data: v}, nil
			}
		}
		specs = append(specs, sp)
	}
	s, rej := connector.New(connector.Options{Specs: specs, Builtin: builtin, Projects: l.projectKeys, Audit: l.store, Logger: l.log})
	for _, r := range rej {
		l.log.Warn("lingo: Config.Tools entry ignored", "index", r.Index, "name", r.Name, "reason", r.Reason)
	}
	return s
}

// maintainDataRequests 定期把租约过期的待完成审计标为中断、清理超过 180 天的审计；只在有可用工具时运行。
func (l *Lingo) maintainDataRequests(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if err := l.data.Maintain(ctx); err != nil && ctx.Err() == nil {
			l.log.Warn("lingo: maintain data request audit failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
