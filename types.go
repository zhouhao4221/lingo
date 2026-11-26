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

// Perspective 是固定的需求视角，由接入方按自身用户身份映射；只决定需求补充归入哪一层（Issue 中按视角分段），
// 与 Role 相互独立，不授予任何权限（PRD 3.2.2.1）。
type Perspective string

const (
	PerspectiveUser     Perspective = "user"     // 使用者：现象、诉求、复现步骤
	PerspectiveBusiness Perspective = "business" // 业务方：业务场景、影响范围、业务价值
	PerspectiveExpert   Perspective = "expert"   // 专家：专业判断、方案建议、风险与约束
	PerspectiveLeader   Perspective = "leader"   // 决策者：做不做、优先级、范围取舍、期限
)

// User 是接入方解析出的请求身份；lingo 不保存用户表，ID 为接入方自己的用户 ID。
type User struct {
	ID       string
	Name     string
	Email    string
	Role     Role
	Scopes   []string // 业务数据权限，与 Tool.Scope 比对
	Projects []string // 可访问的项目 key；为空表示不限制
	// Perspective 是需求视角，为空视为 PerspectiveUser；非法值在提交工单与追加视角补充时返回 ErrInvalidInput。
	Perspective Perspective
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
	// AutoForward 是各工单类型提交后是否自动转出到 Gitea 的默认值；未配置的类型需后台人员确认。
	// admin 可经 SetAutoForward 在运行时覆盖。
	AutoForward map[TicketType]bool
	// RequiredPerspectives 是该项目需求工单的必填视角，覆盖 Config.RequiredPerspectives；nil 表示沿用 Config，
	// 空切片表示该项目不检查。
	RequiredPerspectives []Perspective
}

// TicketType 是工单类型，对应 Gitea 类型标签。
type TicketType string

const (
	TicketConsult TicketType = "consult" // 咨询
	TicketBug     TicketType = "bug"
	TicketFeature TicketType = "feature" // 需求
)

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
	// Assignees 是新建 Issue 默认指派的 Gitea 账号（如研发 Agent）；为空不指派。
	Assignees []string
	// Templates 把工单类型（consult / bug / feature）映射到仓库 Issue 模板文件名，
	// 如 {"bug": "bug_report.yaml"}；未配置的类型按模板标签匹配，仍无则用内置结构。
	Templates map[string]string
	// Deploy 声明如何识别该仓库的版本已上线；全为空时只能经 MarkDeployed 告知。
	Deploy DeploySignal
}

// DeploySignal 声明上线信号；Gitea 没有 Deployment API，任一信号命中即视为上线。
type DeploySignal struct {
	Workflow      string // Gitea Actions 工作流文件名（如 "deploy-prod.yml"），运行成功即上线；需 Gitea ≥ 1.25
	StatusContext string // 提交状态 context（如 "deploy/production"），state=success 即上线
	Env           string // 环境名，记入上线记录；默认 "production"
}

// ReplyMode 决定 Gitea 中哪些评论转给提交人。
type ReplyMode string

const (
	ReplyMarked ReplyMode = "marked" // 默认：只转发 /reply 开头、带 <!-- lingo:public --> 标记或经 MCP 回复的评论
	ReplyAll    ReplyMode = "all"    // 转发全部非机器人评论
)

// GiteaConfig 是 Gitea 连接配置；要求 Gitea ≥ 1.22。
//
// BaseURL、Token、WebhookSecret、AutoRegisterWebhook、WebhookURL、RejectRedirects 都可以不在代码中配置，改为存入数据库，
// 经 UpdateConnectionSettings（REST PUT /settings/connections）修改、无需重启即生效；代码中给了值的项优先，且不能经接口
// 修改（布尔项为 true 时以代码为准，为 false 时读数据库）。地址或令牌都没有时灵工以"未配置"状态运行：Webhook 返回 503，
// 建单等 Gitea 任务暂不执行（不计失败），配置后自动恢复。LabelPrefix 与 ReplyMode 只能在代码中配置。
type GiteaConfig struct {
	BaseURL       string
	Token         string // 机器人账号 token，最小 scope：write:issue、read:repository、read:user
	WebhookSecret string // Webhook 共享密钥

	// LabelPrefix 是来源标签与作用域标签前缀，默认 "lingo"：来源标签即前缀本身，其余为 "<前缀>/type/bug"、
	// "<前缀>/review/need-info" 等。
	LabelPrefix string
	ReplyMode   ReplyMode // 默认 ReplyMarked

	// AutoRegisterWebhook 为真时，Run 启动时（以及连接配置变化后）在各仓库查找或创建指向 WebhookURL 的 Webhook
	// （token 另需 write:repository）；为假时由接入方手工配置。WebhookURL 与 WebhookSecret 可以来自代码或数据库，
	// 生效后缺任何一个时自检不自动注册并报告错误（不会把空密钥写进 Gitea），经接口保存 Gitea 配置时会被拒绝。
	AutoRegisterWebhook bool
	WebhookURL          string // WebhookHandler 的外部可访问地址

	// RejectRedirects 为真时，灵工发往 Gitea 的所有请求（启动自检与全部业务路径）都不跟随 HTTP 重定向：
	// 收到任何 3xx（301/302/303/307/308 等）时不向跳转目标发出任何请求（目标收不到令牌），同主机、子域、跨域与
	// https→http 降级一视同仁；该次调用按不可重试的配置错误失败（outbox 任务直接失败，SelfCheck 对应项为
	// error，错误信息只含方法、路径、状态码与跳转类别，不含跳转地址）。为假（默认）时沿用 Go 标准库行为：
	// 最多跟随 10 次，跨主机时去掉 Authorization，同主机与子域仍携带令牌。只作用于灵工自己的 HTTP 客户端，
	// 不修改 http.DefaultClient 等全局状态。
	RejectRedirects bool
}

// Message 是对话消息。
type Message struct {
	Role    string // system | user | assistant
	Content string
	// Images 是随这条消息发给模型的图片（REQ-009）：只出现在知识问答带图提问的 user 消息中，且只发给声明支持图片的
	// 模型（ImageModel 的 SupportsImages 返回 true）；其他消息恒为空。实现 ImageModel 的模型必须把它们交给模型服务，
	// 不能只按 Content 作答。
	Images []Image
}

// Image 是随消息发给模型的图片（REQ-009），灵工已按内容识别类型并校验大小与尺寸。
type Image struct {
	ContentType string // image/png | image/jpeg | image/webp（按内容识别）
	Data        []byte
}

// ImageModel 是能读图的 Model（可选，REQ-009）：SupportsImages 返回 true 的模型会在 Message.Images 中收到知识问答
// 提问附带的图片。未实现或返回 false 时，带图提问返回 ErrImagesUnsupported（不会丢掉图片只按文字回答）。
// 内置的 NewOpenAIModel 在 OpenAIConfig.Images 为 true 时实现为 true（按 OpenAI 兼容多模态格式发送）。
type ImageModel interface {
	Model
	SupportsImages() bool
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

// Tool 是业务数据工具（REQ-008）：知识问答、MCP 与 CallTool 以调用人身份代其取数。本期只调用 Mode 为 ToolRead 的工具。
//
// 灵工只做第一道防线：调用前校验 User.Scopes 含 Scope；Call 收到的是当前用户，业务侧必须在 Call 内按该用户再次鉴权
// 并过滤数据，并响应 ctx 取消（超时后仍未返回的 Call 会一直占用并发名额，结果被丢弃）。Call 返回的错误不会出现在
// 日志、审计或返回内容中，但也不应携带业务数据。结果按 Sensitive 脱敏、超过 8KB 截断，只用于本次回答或返回，
// 不写入会话、知识库、审计与日志。
//
// 注册校验（New 时）：Name 只含字母、数字、下划线且不超过 64 个字符；Mode 为 ToolRead；Scope 非空；InputSchema 为
// JSON 对象；Call 非空；Timeout 不为负且不超过 60 秒；Scope 不含空白或控制字符（含首尾空格）。不满足的工具被忽略并记 Warn 日志（New 不因此失败）；与内置 MCP
// 工具或先注册的工具同名（不区分大小写）时忽略后者，不覆盖已有工具。
type Tool struct {
	Name        string
	Description string
	// InputSchema 是参数的 JSON Schema（type: object）。调用输入必须整体是一个完整的 JSON 对象；灵工只校验 required 与
	// properties 的基本类型（string / number / integer / boolean / object / array），integer 与 encoding/json 解码到
	// int64 的规则一致（3.0、1e3 不接受）；description 会出现在参数缺失时的追问中。校验通过的输入原样交给 Call。
	InputSchema json.RawMessage
	Scope       string
	Mode        ToolMode
	// Sensitive 是结果中需脱敏的字段路径，如 "phone"、"customer.phone"、"items.*.price"，匹配到的值替换为 "***"；
	// 字段名不区分大小写（"phone" 也匹配未写 json tag 时编码出的 "Phone"），同样作用于审计中的参数摘要。
	Sensitive []string
	// Timeout 是调用时限：0 表示默认 10 秒，最多 60 秒（为负或超过 60 秒时忽略该工具）。
	Timeout time.Duration
	// Call 以 u 的身份查询数据。查无数据时返回（可包装）ErrNotFound，业务侧按 u 拒绝时返回 ErrForbidden：两者是正常的
	// 业务结果，记入审计（拒绝）但不计入熔断，调用方得到与工具不存在相同的结果（不区分无数据与无权）；其他错误视为
	// 技术故障，计入熔断。返回 ToolOutput（或 *ToolOutput）时可在 Project 中声明所查数据的实际归属项目，
	// 审计记录据此归属，负责该项目的 reviewer / admin 可以看到；声明的项目未配置或 u 无权访问时结果被丢弃，
	// 调用方得到与工具不存在相同的结果。返回其他值时视为数据本身，记录归属为空（只有调用人本人可见）。
	Call func(ctx context.Context, u User, input json.RawMessage) (any, error)
}

// ToolOutput 是 Tool.Call 可选的返回值：Project 声明所查数据的实际归属项目（须为 Config.Projects 中的 key），
// Data 为数据本身。
type ToolOutput struct {
	Project string
	Data    any
}

// EventKind 是通知事件类型。
type EventKind string

const (
	EventTicketCreated   EventKind = "ticket.created"
	EventTicketStatus    EventKind = "ticket.status"
	EventTicketComment   EventKind = "ticket.comment"
	EventTicketScheduled EventKind = "ticket.scheduled" // Issue 被排进里程碑，Body 为计划版本与预计日期
	EventReviewDecision  EventKind = "review.decision"
	EventTicketReleased  EventKind = "ticket.released" // 修复版本已发布
	EventTicketDeployed  EventKind = "ticket.deployed" // 修复版本已上线
	// EventTriageRequired 表示工单待后台人员确认转出；UserID 仍为提交人，由接入方决定通知哪些后台人员。
	EventTriageRequired EventKind = "triage.required"
	EventTriageOverdue  EventKind = "triage.overdue"  // 待确认超过 Config.TriageRemindAfter 未处理，按该间隔重复提醒
	EventTriageResolved EventKind = "triage.resolved" // 后台处理完成，见 Event.Action
	// EventTicketPerspectiveAdded 表示需求新增一段视角补充；Perspectives 为本次补充的视角，Body 为提示文案
	// （如“业务方 · 张三 补充了内容”，不含补充内容）。
	EventTicketPerspectiveAdded EventKind = "ticket.perspective_added"
	// EventTicketPerspectiveMissing 表示需求转出时缺少必填视角；Perspectives 为缺失的视角，由接入方决定提醒谁补充。
	EventTicketPerspectiveMissing EventKind = "ticket.perspective_missing"
)

// TriageAction 是后台人员对待确认工单的处理动作。
type TriageAction string

const (
	TriageForward TriageAction = "forward" // 转出到 Gitea
	TriageAnswer  TriageAction = "answer"  // 直接答复并关闭
	TriageMerge   TriageAction = "merge"   // 合并到其他工单
	TriageReject  TriageAction = "reject"  // 驳回
)

// Event 是回传给接入方的工单进展；Notifier 推送与 ListEvents 变更流共用。
// 投递为至少一次，接入方按 ID 去重。
type Event struct {
	ID         int64        `json:"id"` // 全局递增，去重与增量拉取的游标
	Kind       EventKind    `json:"kind"`
	At         time.Time    `json:"at"`
	ProjectKey string       `json:"project"`
	TicketID   int64        `json:"ticket_id,omitempty"`
	TicketType TicketType   `json:"ticket_type,omitempty"`
	Title      string       `json:"title,omitempty"`
	Status     string       `json:"status,omitempty"`
	PrevStatus string       `json:"prev_status,omitempty"` // 状态变化前的状态，非状态类事件为空
	Action     TriageAction `json:"action,omitempty"`      // 仅 triage.resolved
	UserID     string       `json:"user_id,omitempty"`     // 提交人（接入方用户 ID）
	ActorID    string       `json:"actor_id,omitempty"`    // 触发人（接入方用户 ID）；系统或 Gitea 触发为空
	Body       string       `json:"body,omitempty"`
	URL        string       `json:"url,omitempty"` // Gitea Issue 或 Release 链接
	// Perspectives 是视角事件涉及的视角：ticket.perspective_added 为本次补充的视角，ticket.perspective_missing 为缺失的必填视角。
	Perspectives []Perspective `json:"perspectives,omitempty"`
}

// Ticket 是对接入方可见的工单视图（不含 Gitea 内部讨论）。
type Ticket struct {
	ID          int64      `json:"id"`
	ProjectKey  string     `json:"project"`
	Repo        string     `json:"repo,omitempty"` // "owner/name"，未选定仓库时为空
	Type        TicketType `json:"type"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Status      string     `json:"status"`
	UserID      string     `json:"user_id"`
	UserName    string     `json:"user_name,omitempty"`
	IssueURL    string     `json:"issue_url,omitempty"`
	FixedTag    string     `json:"fixed_tag,omitempty"` // 修复版本
	DeployedAt  *time.Time `json:"deployed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	TriageSince *time.Time `json:"triage_since,omitempty"` // 进入待确认的时间，非待确认为空
	Mine        bool       `json:"mine"`                   // 是否调用者本人提交（同事的工单为 false，见 Config.TicketAudience）
}

// TicketAudience 是 Config.TicketAudience 的返回：external 用户额外可见哪些提交人的工单（REQ-004）。
type TicketAudience struct {
	// UserIDs 是额外可见的提交人（接入方用户 ID），如同部门成员。去重后最多取前 MaxTicketAudience 个；含调用者本人时忽略。
	UserIDs []string
}

// MaxTicketAudience 是 TicketAudience.UserIDs 的上限，超出部分丢弃并记一条 Warn 日志。
const MaxTicketAudience = 1000

// VisibleTicketFilter 是 ListVisibleTickets 的条件。
type VisibleTicketFilter struct {
	Project  string // 为空时不限（仍限 User.Projects）
	Status   string
	BeforeID int64 // 游标：上一页最后一条的 ID
	Limit    int   // 0 时默认 50，最多 200
}

// TicketListFilter 是 ListTickets 的条件（REST GET /tickets 同名查询参数），零值表示不限。筛选只在调用者可见范围内收窄，
// 不会扩大范围。
type TicketListFilter struct {
	Project  string     // 项目 key；为空时不限（仍限 User.Projects，User.Projects 为空时为 Config.Projects 全部）
	Repo     string     // 仓库 "owner/name"（不区分大小写）
	Type     TicketType // consult | bug | feature
	Statuses []string   // 任一状态（取值同 Ticket.Status，重复的去重）
	// Keyword 按空白拆成多个关键词，各词须同时命中标题或正文（不区分大小写）；最多 10 个词，每个最多 100 字。
	Keyword string
	// Mine 为真时只含调用者本人提交的（reviewer / admin 收窄到本人；agent 不提交工单，结果为空）。
	Mine bool
	// IncludeShared 只对 external 生效：另含 Config.TicketAudience 放宽可见的同事工单（只读，Mine 为 false）；
	// 默认 external 只含本人提交的。Mine 为真时忽略。
	IncludeShared bool
	// TimeField 是 From / To 比较的时间：created（默认，提交时间）| updated（更新时间）| forwarded（转出时间）。
	TimeField string
	From, To  time.Time // 零值不限；To 不含
	BeforeID  int64     // 游标：上一页的 NextBefore（首页传 0）
	Limit     int       // 0 时默认 50，最多 200
}

// TicketPage 是 ListTickets 的一页结果。
type TicketPage struct {
	Tickets []Ticket `json:"tickets"` // 新的在前（按 ID 降序）
	// Total 是符合条件的工单总数（不受 BeforeID 与 Limit 影响；与 Tickets 分两次查询，并发写入时可能略有出入）。
	Total int `json:"total"`
	// NextBefore 是下一页的 BeforeID；为 0 表示没有下一页。
	NextBefore int64 `json:"next_before"`
}

// IssueClose 是转研发后被驳回的工单关闭关联 Issue 的进度（RejectForwardedTicket 之后由 Run 异步执行）。
type IssueClose struct {
	TicketID int64  `json:"ticket_id"`
	Repo     string `json:"repo,omitempty"`  // 关联 Issue 所在仓库 "owner/name"；没有要关闭的 Issue 时为空
	Number   int64  `json:"issue,omitempty"` // 关联的主 Issue 编号
	// State 取值：none（没有要关闭的 Issue）| pending（排队或重试中）| done（已关闭，或 Issue 已是关闭状态）|
	// failed（超过重试上限或不可重试的错误，含 Issue 返回 404、无法确认已删除；可调用 RetryTicketIssueClose）
	State     string     `json:"state"`
	Attempt   int        `json:"attempt"`              // 第几轮关闭任务，首轮为 1，每次重试入队加 1；没有任务为 0
	Failures  int        `json:"failures"`             // 本轮已失败次数
	LastError string     `json:"last_error,omitempty"` // 本轮最近一次失败原因（脱敏说明：类别与 HTTP 状态码，不含地址与响应正文）
	UpdatedAt *time.Time `json:"updated_at,omitempty"` // 本轮任务最近更新时间
}

// Comment 是工单评论：提交人补充、后台答复、研发在 Gitea 的回复。external 只能看到对外评论。
type Comment struct {
	ID         int64     `json:"id"`
	Source     string    `json:"source"` // user（提交人）| staff（后台人员）| agent（研发 Agent 经 MCP 回复）| gitea（Gitea 评论）
	AuthorID   string    `json:"author_id,omitempty"`
	AuthorName string    `json:"author_name,omitempty"` // 来自 Gitea 时为研发账号 login
	Body       string    `json:"body"`
	Public     bool      `json:"public"`
	At         time.Time `json:"at"`
}

// Attachment 是工单或补充评论的附件引用，也是知识问答提问附带的图片引用（REQ-009，见 MessageInput.Attachments，
// 不带 URL）。文件由接入方自己的附件系统保存和鉴权，灵工只记录引用：提交工单或补充时传入 ID、Name、URL（及可选的
// ContentType、Size），转出时写进 Gitea Issue；配置了 Config.FetchAttachment 时读取内容上传为 Gitea 附件。
type Attachment struct {
	ID          string `json:"id"`                     // 接入方的附件 ID，必填，在整个工单（含补充评论）内唯一
	Name        string `json:"name"`                   // 文件名；为空时取 ID
	URL         string `json:"url,omitempty"`          // http(s) 绝对地址，写进 Issue 供研发查看（访问控制由接入方负责）
	ContentType string `json:"content_type,omitempty"` // 图片类型上传到 Gitea 后在 Issue 中直接显示
	Size        int64  `json:"size,omitempty"`
	// 以下只在读取时返回
	CommentID int64  `json:"comment_id,omitempty"` // 随补充评论提交时为评论 ID
	Forwarded bool   `json:"forwarded"`            // 已转出到 Gitea（上传、写入链接或写明失败）
	GiteaURL  string `json:"gitea_url,omitempty"`  // 已上传为 Gitea 附件时的地址
	Error     string `json:"error,omitempty"`      // 读取或上传失败的原因

	// 以下只在调用 Config.FetchAttachment 时填写，不出现在 JSON 中（REQ-009）：接入方据此区分用途并核对归属。
	Purpose        AttachmentPurpose `json:"-"`
	TicketID       int64             `json:"-"` // Purpose 为 ticket：所属工单（CommentID 非 0 时随该补充评论提交）
	ConversationID int64             `json:"-"` // Purpose 为 question：所属问答会话
	MessageID      int64             `json:"-"` // Purpose 为 question：附带该图片的用户消息
}

// AttachmentPurpose 是 Config.FetchAttachment 读取附件的用途（REQ-009）。
type AttachmentPurpose string

const (
	// AttachmentForTicket：工单或补充评论的附件，转出到 Gitea（上传为 Issue / 评论附件，研发可见）。
	AttachmentForTicket AttachmentPurpose = "ticket"
	// AttachmentForQuestion：知识问答提问附带的图片，发给回答模型（不转出到 Gitea、不进入知识库）。userID 是提问人
	// （会话创建者）；接入方应核对该附件由此人上传，且允许发给模型服务。
	AttachmentForQuestion AttachmentPurpose = "question"
)

// CheckResult 是一项启动自检结果；Repo 为空表示实例级检查。Level 为 ok / warn（功能受限）/ error（该仓库无法对接）。
type CheckResult struct {
	Repo    string `json:"repo,omitempty"`
	Item    string `json:"item"` // config | version | attachments | repo | permission | labels | webhook | delivery | templates | workflow
	Level   string `json:"level"`
	Message string `json:"message"`
}

// ProjectInfo 是调用者可访问的项目及其仓库。
type ProjectInfo struct {
	Key         string     `json:"key"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	DefaultRepo string     `json:"default_repo,omitempty"`
	Repos       []RepoInfo `json:"repos"`
}

// RepoInfo 是项目下的仓库。
type RepoInfo struct {
	Repo string   `json:"repo"` // "owner/name"
	Kind RepoKind `json:"kind"`
}

// Release 是同步自 Gitea 的版本记录（或只打了 tag 的版本）。
type Release struct {
	ID          int64     `json:"id"`
	Repo        string    `json:"repo"`
	Tag         string    `json:"tag"`
	Name        string    `json:"name,omitempty"`
	Body        string    `json:"body,omitempty"` // 版本说明
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	URL         string    `json:"url,omitempty"`
}

// ReleaseDetail 是版本详情：关联的工单（按调用者可见性过滤）与上线记录。
type ReleaseDetail struct {
	Release
	Tickets     []Ticket     `json:"tickets"`
	Deployments []Deployment `json:"deployments"`
}

// Deployment 是一条上线记录。
type Deployment struct {
	Env    string    `json:"env"`
	Source string    `json:"source"` // workflow | status | manual
	At     time.Time `json:"at"`
}

// TicketTypeInfo 是某类工单可填写的表单，取自仓库 Issue 表单模板；没有模板时 Fields 为空（只填正文）。
type TicketTypeInfo struct {
	Type     TicketType  `json:"type"`
	Template string      `json:"template,omitempty"` // 模板名称
	Fields   []FormField `json:"fields,omitempty"`
}

// FormField 是表单模板中的一个输入字段，提交时按 ID 放入 TicketInput.Fields。
type FormField struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"` // input | textarea | dropdown | checkboxes
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Required    bool     `json:"required"`
	Options     []string `json:"options,omitempty"`
	Multiple    bool     `json:"multiple,omitempty"`
}

// AutoForwardSetting 是项目 + 工单类型当前生效的自动转出开关。
type AutoForwardSetting struct {
	ProjectKey string     `json:"project"`
	Type       TicketType `json:"type"`
	On         bool       `json:"on"`
	Overridden bool       `json:"overridden"` // 为真表示是运行时覆盖值，否则为 Project.AutoForward 默认值
}

// TriageFilter 是待确认列表的过滤条件。
type TriageFilter struct {
	ProjectKey    string
	Type          TicketType
	PendingBefore time.Time // 非零时只返回在该时间之前进入待确认的工单
	AfterID       int64     // 游标分页
	Limit         int       // 0 表示默认 50
}

// TriageCount 是单个项目的待确认统计。
type TriageCount struct {
	ProjectKey string `json:"project"`
	Pending    int    `json:"pending"`
	Overdue    int    `json:"overdue"` // 超过 Config.TriageRemindAfter 的数量
}

// Notifier 接收工单进展事件，经 outbox 投递，返回错误会重试；由接入方决定发邮件、站内信等。
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}
