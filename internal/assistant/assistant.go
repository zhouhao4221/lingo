package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zhouhao4221/lingo/internal/knowledge"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/connector"
	"github.com/zhouhao4221/lingo/internal/release"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// ErrNoModel 表示未配置模型，发送与重试消息不可用（会话列表、历史、确认与取消不受影响）。
var ErrNoModel = errors.New("assistant: model not configured")

// 消息角色与状态。
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"

	StatusDone   = "done"
	StatusFailed = "failed"
)

// 失败类别（Message.Error）。
const (
	ErrKindModel    = "model_error"
	ErrKindTimeout  = "timeout"
	ErrKindCanceled = "canceled"
	ErrKindInternal = "internal_error" // 落库等内部错误
)

// 待确认操作的种类与状态。
const (
	ActionTicketComment = "ticket_comment"

	ActionPending   = "pending"
	ActionExecuted  = "executed"
	ActionCancelled = "cancelled"
	ActionExpired   = "expired"
	ActionFailed    = "failed" // 确认时被拒（无权限、工单状态不允许）
)

// 意图种类（Intent.Kind，也记入助手消息的 intent 供统计）。
const (
	IntentTicketFind    = "ticket_find"
	IntentTicketSummary = "ticket_summary"
	IntentReleaseList   = "release_list"
	IntentTicketFix     = "ticket_fix"
	IntentComment       = "comment"
	IntentConfirm       = "confirm"
	IntentCancel        = "cancel"
	IntentClarify       = "clarify"
	IntentOutOfScope    = "out_of_scope"
	IntentKnowledgeAsk  = "knowledge_ask" // 使用方法、原因、怎么做等知识类问题（REQ-006）
	IntentDataQuery     = "data_query"    // 需要调用业务数据工具回答的问题（REQ-008）

	// IntentOtherDecision 是 Brain.Confirm 的第三种结果：用户在说别的事情。
	IntentOtherDecision = "other"
)

// 结果分类（助手消息的 outcome，供统计：答不上 = not_found + out_of_scope，留言 = action_executed）。
const (
	OutcomeAnswered        = "answered"
	OutcomeCandidates      = "candidates"
	OutcomeNotFound        = "not_found"
	OutcomeOutOfScope      = "out_of_scope"
	OutcomeClarify         = "clarify"
	OutcomeActionProposed  = "action_proposed"
	OutcomeActionExecuted  = "action_executed"
	OutcomeActionCancelled = "action_cancelled"
	OutcomeActionExpired   = "action_expired"
	OutcomeActionFailed    = "action_failed"
	OutcomeNoAction        = "no_action"
	OutcomeDataFailed      = "data_failed" // 业务数据查询失败或暂时不可用（REQ-008）
)

// 限制。
const (
	MaxContentRunes = 4000             // 单条提问的长度上限
	ActionTTL       = 30 * time.Minute // 待确认操作的有效期
	HistoryTurns    = 10               // 带给模型的最近轮数
	MaxTickets      = 20               // 单次交给模型的工单上限
	MaxReleases     = 10               // 单次交给模型的版本上限
	turnTimeout     = 2 * time.Minute  // 一轮提问（含模型调用）的时限
	// turnLease 是生成租约：回答时限，加上时限到达时可能还在进行的一次推送（SSE 写期限 30 秒）与落库（10 秒），再留余量
	turnLease = turnTimeout + 80*time.Second
)

// Conversation 是一个问答会话，只有创建者本人可访问。
type Conversation struct {
	ID           int64
	UserID       string
	Title        string
	Seq          int64 // 最近活动序号：列表按它倒序，也是分页游标
	LastActiveAt time.Time
	CreatedAt    time.Time
	// Lease 是本次取得的生成租约（ClaimTurn 回填），释放时只清除自己持有的租约
	Lease time.Time
}

// Message 是会话中的一条消息。
type Message struct {
	ID             int64
	ConversationID int64
	Role           string
	Content        string
	Status         string
	Error          string
	ReplyTo        int64 // 助手消息所回答的用户消息
	Intent         string
	Outcome        string
	Citations      Citations
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// ModelOK 是助手消息生成时送给模型的工单评论、知识条目与历史回答是否都在 ReplyMarked 内容范围内（可信来源链，
	// REQ-012）：nil 为未知（升级前的消息、失败的回答），用户消息恒为 nil（提问是用户自己的内容）。
	ModelOK *bool
	Action  *Action // 该助手消息提出的待确认操作（读取时回填）
	// Attachments 是用户消息附带的图片引用（REQ-009）；助手消息为空
	Attachments []Attachment
}

// Citations 是回答引用的工单、版本与知识条目（回答当时的快照）。
type Citations struct {
	Tickets   []TicketRef    `json:"tickets"`
	Releases  []ReleaseRef   `json:"releases"`
	Knowledge []KnowledgeRef `json:"knowledge,omitempty"`
	// Tools 是回答调用的业务数据工具（REQ-008），只记工具名，不保存结果
	Tools []ToolRef `json:"tools,omitempty"`
}

// ToolRef 是回答调用的业务数据工具。
type ToolRef struct {
	Name string `json:"name"`
}

// ToolInfo 是交给意图解析的业务数据工具（调用人可调用的，描述已截断）。
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// DataTools 是业务数据工具（REQ-008），由根包适配；调用人由根包放在 ctx 中。Run 返回 connector 的错误
// （connector.ErrNotFound、*connector.InputError、connector.ErrUnavailable、connector.ErrFailed）。
type DataTools interface {
	Visible(ctx context.Context) []ToolInfo
	Run(ctx context.Context, name string, input json.RawMessage, convID int64) (*connector.Result, error)
}

// 交给意图解析的工具数量与描述长度上限。
const (
	MaxIntentTools = 30
	toolDescRunes  = 300
)

// KnowledgeRef 是引用的知识条目；SourceTicketID 只在调用者能查看来源工单时填写。
type KnowledgeRef struct {
	ID             int64  `json:"id"`
	Question       string `json:"question"`
	SourceTicketID int64  `json:"source_ticket_id,omitempty"`
}

// Knowledge 是知识库检索，由 knowledge.Service 实现。
type Knowledge interface {
	// SearchForModel 检索交给模型的知识条目：启用模型内容策略时只含来源可信的条目（Hit.ModelInput，REQ-012）。
	SearchForModel(ctx context.Context, p auth.Principal, project, query string, keywords []string, limit int) ([]knowledge.Hit, error)
	ByIDs(ctx context.Context, ids []int64) ([]*knowledge.Entry, error)
	Cite(ctx context.Context, ids []int64) error
	// Suggest 以待审核状态入库回答候选（modelOK 为该回答的 Message.ModelOK）；Miss 记录未答上的提问（REQ-007）。
	Suggest(ctx context.Context, d knowledge.Draft, messageID int64, modelOK *bool) error
	Miss(ctx context.Context, p auth.Principal, project, question string) error
}

// TicketRef 是引用的工单。
type TicketRef struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	IssueURL string `json:"issue_url,omitempty"`
}

// ReleaseRef 是引用的版本。
type ReleaseRef struct {
	ID          int64     `json:"id"`
	Project     string    `json:"project"`
	Repo        string    `json:"repo"`
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"published_at"`
	URL         string    `json:"url,omitempty"`
}

// Action 是待确认操作：灵工在对话中提出、用户确认后才执行的写操作。
type Action struct {
	ID             int64
	ConversationID int64
	MessageID      int64
	UserID         string
	Kind           string
	Payload        json.RawMessage
	Summary        string
	Status         string
	Result         json.RawMessage
	ExpiresAt      time.Time
	ResolvedAt     *time.Time
	CreatedAt      time.Time
}

// CommentPayload 是 ticket_comment 操作的参数。
type CommentPayload struct {
	TicketID int64  `json:"ticket_id"`
	Body     string `json:"body"`
}

// Store 是问答仓储，由 internal/store 实现。会话读取一律带 userID，他人的会话按不存在处理。
type Store interface {
	InAssistantTx(ctx context.Context, fn func(Tx) error) error
	CreateConversation(ctx context.Context, userID, title string) (*Conversation, error)
	// ListConversations 返回本人未删除的会话，按最近活动倒序；beforeSeq 为游标（0 不限）。
	ListConversations(ctx context.Context, userID string, beforeSeq int64, limit int) ([]*Conversation, error)
	// GetConversation 返回本人未删除的会话；不存在返回 ticket.ErrNotFound。
	GetConversation(ctx context.Context, userID string, id int64) (*Conversation, error)
	// DeleteConversation 软删除本人的会话；不存在返回 ticket.ErrNotFound。
	DeleteConversation(ctx context.Context, userID string, id int64) error
	// Messages 返回会话中 id < beforeID（0 不限）的最近 limit 条消息，按 ID 升序，回填各自提出的待确认操作。
	Messages(ctx context.Context, conversationID, beforeID int64, limit int) ([]*Message, error)
	// RecentDone 返回会话中 id < beforeID 的最近 n 条已完成消息，按 ID 升序。
	RecentDone(ctx context.Context, conversationID, beforeID int64, n int) ([]*Message, error)
}

// Tx 是事务内的问答操作，同时具备工单操作（确认留言与会话写入同事务提交）。
type Tx interface {
	ticket.Tx
	// ClaimTurn 为会话写入生成租约（数据库时间 now() + hold），回填 conv.Lease；租约未过期时 ok 为假；会话不存在时
	// 返回 ticket.ErrNotFound。
	ClaimTurn(ctx context.Context, userID string, id int64, hold time.Duration) (conv *Conversation, ok bool, err error)
	// ReleaseTurn 清除生成租约，只在租约仍是 lease（自己持有的那个）时清除：超时后被别人取得的租约不受影响。
	ReleaseTurn(ctx context.Context, id int64, lease time.Time) error
	// Touch 刷新最近活动（取新的活动序号）；会话标题为空且 title 非空时写入标题。
	Touch(ctx context.Context, id int64, title string) error
	InsertMessage(ctx context.Context, m *Message) error
	// UpdateMessage 保存助手消息的内容、状态、错误、意图、结果与引用。
	UpdateMessage(ctx context.Context, m *Message) error
	// SetFeedback 写入（覆盖）助手消息的反馈，返回之前的反馈（没有时为 nil）。
	SetFeedback(ctx context.Context, messageID int64, userID string, helpful bool) (*bool, error)
	// AddKnowledgeFeedback 给知识条目累加有帮助 / 没帮助次数（与反馈同一事务）。
	AddKnowledgeFeedback(ctx context.Context, ids []int64, helpful, unhelpful int) error
	// GetMessage 读取会话中的消息；不存在返回 ticket.ErrNotFound。
	GetMessage(ctx context.Context, conversationID, id int64) (*Message, error)
	// LastUserMessageID 返回会话最后一条用户消息的 ID（没有时为 0）。
	LastUserMessageID(ctx context.Context, conversationID int64) (int64, error)
	// PendingAction 锁定并返回会话中状态为 pending 的操作；没有时返回 nil。
	PendingAction(ctx context.Context, conversationID int64) (*Action, error)
	// LockAction 锁定并返回会话中的操作；不存在返回 ticket.ErrNotFound。
	LockAction(ctx context.Context, conversationID, id int64) (*Action, error)
	InsertAction(ctx context.Context, a *Action) error
	// ResolveAction 保存操作的状态、结果与处理时间。
	ResolveAction(ctx context.Context, a *Action) error
	// Savepoint / RollbackToSavepoint 在事务中设置保存点与回滚到保存点（执行器被拒时撤销其部分写入）。
	Savepoint(ctx context.Context, name string) error
	RollbackToSavepoint(ctx context.Context, name string) error
	// UserMessageBetween 报告会话中 ID 在 (after, before) 之间是否有用户消息。
	UserMessageBetween(ctx context.Context, conversationID, after, before int64) (bool, error)
}

// ProjectInfo 是调用者可访问的项目，供意图解析选择项目与仓库。
type ProjectInfo struct {
	Key   string   `json:"key"`
	Name  string   `json:"name"`
	Repos []string `json:"repos"`
}

// Releases 是版本查询，由 release.Service 实现。
type Releases interface {
	List(ctx context.Context, p auth.Principal, q release.Query) ([]*release.Release, error)
	Count(ctx context.Context, p auth.Principal, q release.Query) (int, error)
	Get(ctx context.Context, p auth.Principal, id int64, tickets func(ctx context.Context, ids []int64) ([]*ticket.Ticket, error)) (*release.Detail, error)
}

// Options 是 Service 的配置。
type Options struct {
	Store    Store
	Tickets  *ticket.Service
	Releases Releases
	// TicketsByID 一次读取多张工单（不鉴权，供版本详情读取关联工单后按 Visible 过滤）。
	TicketsByID func(ctx context.Context, ids []int64) ([]*ticket.Ticket, error)
	// Knowledge 是知识库检索（REQ-006）；为空时知识类问题按超出范围回答。
	Knowledge Knowledge
	// DataTools 是业务数据工具（REQ-008）；为空时意图解析看不到工具，行为与未接入时一致。
	DataTools DataTools
	// FetchImage 读取提问附带的图片（REQ-009）；为空时带图提问返回 ErrImagesUnsupported。
	FetchImage FetchImage
	// Brain 是模型调用；为空时发送与重试返回 ErrNoModel。
	Brain Brain
	// BrainFor 非空时每轮提问开始时调用，取当前的模型（运行时可更换模型），返回 nil 表示未配置；优先于 Brain。
	BrainFor func() Brain
	// Projects 返回调用者可访问的项目（仅当前 Config 中声明的）。
	Projects func(p auth.Principal) []ProjectInfo
	// ProjectOfRepo 返回仓库（"owner/name"）所属项目 key。
	ProjectOfRepo func(repo string) string
	// IssueURL 返回 Issue 的地址，用于工单引用。
	IssueURL func(repo string, number int64) string
	Now      func() time.Time
	// Location 是未指定时区时解析"今天 / 上周"等用的时区，默认 time.Local。
	Location *time.Location
	// ModelMarked 为真时启用模型内容策略 ModelContentMarked（REQ-012）：external 的问答只把 Comment.Model 的评论交给模型；
	// 历史中来源未知（ModelOK 为 nil）的回答、以及 external 的 ModelOK 为 false 的回答不作为模型输入（只保留引用的工单与
	// 版本，用于理解指代）。知识检索经 Knowledge.SearchForModel。为假时行为与升级前相同（仍记录 ModelOK）。
	ModelMarked bool
}

// Service 是问答会话与回答。
type Service struct {
	o     Options
	execs map[string]executor
}

// New 创建 Service。
func New(o Options) (*Service, error) {
	if o.Store == nil || o.Tickets == nil || o.Releases == nil || o.TicketsByID == nil {
		return nil, errors.New("assistant: Store, Tickets, Releases and TicketsByID are required")
	}
	if o.Projects == nil {
		o.Projects = func(auth.Principal) []ProjectInfo { return nil }
	}
	if o.ProjectOfRepo == nil {
		o.ProjectOfRepo = func(string) string { return "" }
	}
	if o.IssueURL == nil {
		o.IssueURL = func(string, int64) string { return "" }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	s := &Service{o: o}
	s.execs = map[string]executor{ActionTicketComment: s.execComment}
	return s, nil
}

// Brain 是问答用到的模型调用，由 internal/llm 实现。模型出错时返回错误；输出不合格时不报错，
// 按保守的结果返回（Confirm 返回 "other"，Intent 返回 clarify）。
type Brain interface {
	// Confirm 判断用户这条消息对待确认操作的态度：confirm | cancel | other。只看操作摘要与这条消息。
	Confirm(ctx context.Context, in ConfirmInput) (string, error)
	// Intent 把提问解析为查询意图。看不到本轮查询到的数据。
	Intent(ctx context.Context, in IntentInput) (*Intent, error)
	// Answer 依据 Facts 组织回答；onDelta 非空时逐片段回调（模型不支持流式时整段回调一次）。
	Answer(ctx context.Context, in AnswerInput, onDelta func(string)) (string, error)
	// SupportsImages 报告能否处理带图提问（REQ-009）：组织回答的模型声明了支持图片。为假时带图提问返回
	// ErrImagesUnsupported，不会丢掉图片只按文字回答。
	SupportsImages() bool
}

// ConfirmInput 是确认判断的输入。
type ConfirmInput struct {
	Summary string // 待确认操作的描述
	Message string // 用户的这条消息
}

// HistoryItem 是带给模型的一条历史消息。
type HistoryItem struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Tickets   []TicketRef    `json:"tickets,omitempty"`
	Knowledge []KnowledgeRef `json:"knowledge,omitempty"`
	Tags      []string       `json:"releases,omitempty"`
	// Images 是该条用户消息附带的图片数（REQ-009）；历史图片不再发给模型
	Images int `json:"images,omitempty"`
}

// IntentInput 是意图解析的输入。
type IntentInput struct {
	Today     string // 如 "2026-11-24 星期二"，供模型理解相对时间（日期由本包换算）
	Projects  []ProjectInfo
	History   []HistoryItem
	Question  string
	NeedTitle bool // 需要同时生成会话标题
	// Tools 是调用人可调用的业务数据工具（REQ-008）；为空时意图解析的提示词与输入与未接入时完全相同。
	Tools []ToolInfo
	// Images 是本条提问附带的图片（REQ-009），已校验；为空时意图解析的提示词与输入与不带图时完全相同。
	Images []Image
}

// Intent 是解析出的查询意图。字段已由 Brain 校验（非法枚举丢弃、数量与长度截断）。
type Intent struct {
	Kind           string
	TicketIDs      []int64
	IssueNumber    int64
	IssueExplicit  bool // 用户明确说的是 Gitea Issue 编号（"Issue #N"、"owner/name#N"）
	Project        string
	UnknownProject bool // 提到的项目不在可访问列表中
	Repo           string
	Type           string
	Statuses       []string
	Mine           bool
	Keywords       []string
	Time           TimeSpec
	Release        ReleaseSpec
	CommentBody    string
	Clarify        string
	Title          string
	Tool           string          // data_query：工具名（已校验在 IntentInput.Tools 中）
	ToolInput      json.RawMessage // data_query：参数对象
}

// TimeSpec 是意图中的时间范围：Preset 或 From / To（YYYY-MM-DD，To 含当天）。
type TimeSpec struct {
	Field  string // created | updated | forwarded
	Preset string // today | yesterday | this_week | last_week | this_month | last_month | last_7_days | last_30_days
	From   string
	To     string
}

// ReleaseSpec 是意图中指定的版本。
type ReleaseSpec struct {
	Repo string
	Tag  string
}

// AnswerInput 是组织回答的输入。
type AnswerInput struct {
	History  []HistoryItem
	Question string
	Facts    json.RawMessage // 本轮查询结果（白名单字段）
	Images   []Image         // 本条提问附带的图片（REQ-009）
}
