package lingo

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

// Config 是 lingo 的全部配置，由接入方在代码中构造。
type Config struct {
	DB           *sql.DB // PostgreSQL，驱动由接入方选择
	Model        Model
	Gitea        GiteaConfig
	Projects     []Project // 第一个为默认项目（请求未指定 project 时使用）
	Authenticate Authenticator
	Notifier     Notifier // 可空，为空时不回传进展
	Tools        []Tool
	// MaxAttachmentBytes 是单个附件大小上限，0 表示默认 10 MB。
	MaxAttachmentBytes int64
	// TriageRemindAfter 是待确认工单的提醒间隔：超过该时长未处理发出 triage.overdue，之后按同样间隔重复；
	// 0 表示默认 24 小时，负数关闭提醒（接入方可用 ListTriage 的 PendingBefore 自行实现）。
	TriageRemindAfter time.Duration
}

// Lingo 是一个灵问实例。
type Lingo struct {
	cfg Config
}

// New 校验配置并创建实例；不执行数据库迁移。
func New(cfg Config) (*Lingo, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("lingo: Config.DB is required")
	case cfg.Model == nil:
		return nil, errors.New("lingo: Config.Model is required")
	case cfg.Gitea.BaseURL == "" || cfg.Gitea.Token == "":
		return nil, errors.New("lingo: Config.Gitea.BaseURL and Token are required")
	case cfg.Authenticate == nil:
		return nil, errors.New("lingo: Config.Authenticate is required")
	case len(cfg.Projects) == 0:
		return nil, errors.New("lingo: at least one Project is required")
	case cfg.Gitea.AutoRegisterWebhook && cfg.Gitea.WebhookURL == "":
		return nil, errors.New("lingo: Config.Gitea.WebhookURL is required when AutoRegisterWebhook is set")
	}
	return &Lingo{cfg: cfg}, nil
}

// Handler 返回 REST JSON 接口（提问、工单、评审、知识库、版本），路径为相对路径，
// 由接入方挂载到任意前缀（配合 http.StripPrefix）。
func (l *Lingo) Handler() http.Handler { return notImplemented() }

// MCPHandler 返回 MCP Server（Streamable HTTP），工具与 Handler 能力对齐。
func (l *Lingo) MCPHandler() http.Handler { return notImplemented() }

// WebhookHandler 接收 Gitea Webhook（Issue、评论、PR、Release、提交状态、Actions 运行），验签并按事件去重。
func (l *Lingo) WebhookHandler() http.Handler { return notImplemented() }

// SetAutoForward 在运行时覆盖项目 project 下工单类型 t 是否自动转出（仅 admin），存入数据库并优先于
// Project.AutoForward；u 为操作人，用于权限校验与留痕。
func (l *Lingo) SetAutoForward(ctx context.Context, u User, project string, t TicketType, on bool) error {
	return errNotImplemented
}

// ResetAutoForward 删除运行时覆盖，恢复为 Project.AutoForward 的默认值（仅 admin）。
func (l *Lingo) ResetAutoForward(ctx context.Context, u User, project string, t TicketType) error {
	return errNotImplemented
}

// GetTicket 返回工单 id；无权访问时与不存在一样返回 ErrNotFound（见数据隔离）。
func (l *Lingo) GetTicket(ctx context.Context, u User, id int64) (Ticket, error) {
	return Ticket{}, errNotImplemented
}

// ListTriage 列出 u 可见项目中的待确认工单（仅 reviewer / admin）。
func (l *Lingo) ListTriage(ctx context.Context, u User, f TriageFilter) ([]Ticket, error) {
	return nil, errNotImplemented
}

// TriageSummary 返回 u 可见项目的待确认数与超时数（仅 reviewer / admin）。
func (l *Lingo) TriageSummary(ctx context.Context, u User) ([]TriageCount, error) {
	return nil, errNotImplemented
}

// ListEvents 返回 ID 大于 afterID 的事件（按 ID 升序，最多 limit 条，0 表示默认 100），
// 与 Notifier 推送的事件同源；按 u 的角色与项目过滤。
func (l *Lingo) ListEvents(ctx context.Context, u User, afterID int64, limit int) ([]Event, error) {
	return nil, errNotImplemented
}

// MarkDeployed 告知灵问仓库 repo（"owner/name"）的版本 tag 已在环境 env 上线，
// 关联工单变为已上线并通知提交人；用于部署流水线不经 Gitea Actions / 提交状态上报的场景。
func (l *Lingo) MarkDeployed(ctx context.Context, repo, tag, env string) error {
	return errNotImplemented
}

// Run 运行后台任务（outbox 投递、Gitea 初始化与对账补偿、知识库生成），直到 ctx 取消；多实例运行安全。
func (l *Lingo) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// ErrNotFound 表示资源不存在或无权访问（两者不区分，避免暴露数据是否存在）。
var ErrNotFound = errors.New("lingo: not found")

var errNotImplemented = errors.New("lingo: not implemented")

// TODO: 骨架阶段，接口待实现。
func notImplemented() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "lingo: not implemented", http.StatusNotImplemented)
	})
}
