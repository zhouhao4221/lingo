package lingo

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Role 是固定角色，由接入方把自己的用户映射到其中之一。
type Role string

const (
	RoleExternal Role = "external" // 提问与提交工单的终端用户
	RoleAgent    Role = "agent"    // AI 研发 Agent 服务账号
	RoleReviewer Role = "reviewer" // 研发负责人，负责需求评审
	RoleAdmin    Role = "admin"    // 管理知识库审核等
)

// User 是接入方解析出的请求身份；lingo 不保存用户表，ID 为接入方自己的用户 ID。
type User struct {
	ID       string
	Name     string
	Email    string
	Role     Role
	Scopes   []string // 业务数据权限，与 Tool.Scope 比对
	Projects []string // 可访问的项目 key；为空表示不限制
}

// Authenticator 从 HTTP 请求解析身份，供 Handler 与 MCPHandler 使用。
type Authenticator func(r *http.Request) (User, error)

// Project 是对接单位，含多个 Gitea 仓库；在 Config 中声明，New 时同步入库。
type Project struct {
	Key         string
	Name        string
	Description string
	Repos       []Repo
	DefaultRepo string // 默认仓库 "owner/name"，未指定时取 Repos[0]
}

// RepoKind 是仓库类型，用于判定 Issue 落库仓库。
type RepoKind string

const (
	RepoFrontend RepoKind = "frontend"
	RepoBackend  RepoKind = "backend"
	RepoMobile   RepoKind = "mobile"
	RepoDocs     RepoKind = "docs"
	RepoOther    RepoKind = "other"
)

// Repo 是 Gitea 仓库，机器人账号需有 Issue 读写权限。
type Repo struct {
	Owner string
	Name  string
	Kind  RepoKind
}

// GiteaConfig 是 Gitea 连接配置。
type GiteaConfig struct {
	BaseURL       string
	Token         string // 机器人账号 token
	WebhookSecret string // Webhook 共享密钥
}

// Message 是对话消息。
type Message struct {
	Role    string // system | user | assistant
	Content string
}

// Model 是大模型与向量服务，由接入方实现或使用适配器。
type Model interface {
	Chat(ctx context.Context, messages []Message) (string, error)
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ToolMode 区分只读与操作类业务数据工具。
type ToolMode string

const (
	ToolRead  ToolMode = "read"
	ToolWrite ToolMode = "write" // 执行前需用户确认
)

// Tool 是业务数据工具：问答时以提问人员身份代其取数。
// lingo 预校验 User.Scopes 含 Scope，Call 内由业务侧二次校验；结果仅用于本次会话、不入知识库。
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema
	Scope       string
	Mode        ToolMode
	Sensitive   []string // 返回结果中需脱敏的字段
	Call        func(ctx context.Context, u User, input json.RawMessage) (any, error)
}

// EventKind 是通知事件类型。
type EventKind string

const (
	EventTicketCreated  EventKind = "ticket.created"
	EventTicketStatus   EventKind = "ticket.status"
	EventTicketComment  EventKind = "ticket.comment"
	EventReviewDecision EventKind = "review.decision"
	EventTicketReleased EventKind = "ticket.released" // 修复版本已发布
)

// Event 是回传给接入方的工单进展。
type Event struct {
	Kind       EventKind
	UserID     string // 提交人（接入方用户 ID）
	ProjectKey string
	TicketID   int64
	Title      string
	Status     string
	Body       string
	URL        string // Gitea Issue 或 Release 链接
	At         time.Time
}

// Notifier 接收工单进展事件，经 outbox 投递，返回错误会重试；由接入方决定发邮件、站内信等。
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}
