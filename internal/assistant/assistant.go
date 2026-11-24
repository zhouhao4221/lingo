package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
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
)

// 限制。
const (
	MaxContentRunes = 4000             // 单条提问的长度上限
	ActionTTL       = 30 * time.Minute // 待确认操作的有效期
	HistoryTurns    = 10               // 带给模型的最近轮数
	MaxTickets      = 20               // 单次交给模型的工单上限
	MaxReleases     = 10               // 单次交给模型的版本上限
	turnTimeout     = 2 * time.Minute  // 一轮提问（含模型调用）的时限
	turnLease       = turnTimeout + 30*time.Second
)

// Conversation 是一个问答会话，只有创建者本人可访问。
type Conversation struct {
	ID           int64
	UserID       string
	Title        string
	Seq          int64 // 最近活动序号：列表按它倒序，也是分页游标
	LastActiveAt time.Time
	CreatedAt    time.Time
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
	Action         *Action // 该助手消息提出的待确认操作（读取时回填）
}

// Citations 是回答引用的工单与版本（回答当时的快照）。
type Citations struct {
	Tickets  []TicketRef  `json:"tickets"`
	Releases []ReleaseRef `json:"releases"`
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
	// ClaimTurn 为会话写入生成租约（数据库时间 now() + hold），租约未过期时 ok 为假；会话不存在时返回 ticket.ErrNotFound。
	ClaimTurn(ctx context.Context, userID string, id int64, hold time.Duration) (conv *Conversation, ok bool, err error)
	// ReleaseTurn 清除生成租约。
	ReleaseTurn(ctx context.Context, id int64) error
	// Touch 刷新最近活动（取新的活动序号）；会话标题为空且 title 非空时写入标题。
	Touch(ctx context.Context, id int64, title string) error
	InsertMessage(ctx context.Context, m *Message) error
	// UpdateMessage 保存助手消息的内容、状态、错误、意图、结果与引用。
	UpdateMessage(ctx context.Context, m *Message) error
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
	// Brain 是模型调用；为空时发送与重试返回 ErrNoModel。
	Brain Brain
	// Projects 返回调用者可访问的项目（仅当前 Config 中声明的）。
	Projects func(p auth.Principal) []ProjectInfo
	// ProjectOfRepo 返回仓库（"owner/name"）所属项目 key。
	ProjectOfRepo func(repo string) string
	// IssueURL 返回 Issue 的地址，用于工单引用。
	IssueURL func(repo string, number int64) string
	Now      func() time.Time
	// Location 是未指定时区时解析"今天 / 上周"等用的时区，默认 time.Local。
	Location *time.Location
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
}

// ConfirmInput 是确认判断的输入。
type ConfirmInput struct {
	Summary string // 待确认操作的描述
	Message string // 用户的这条消息
}

// HistoryItem 是带给模型的一条历史消息。
type HistoryItem struct {
	Role    string      `json:"role"`
	Content string      `json:"content"`
	Tickets []TicketRef `json:"tickets,omitempty"`
	Tags    []string    `json:"releases,omitempty"`
}

// IntentInput 是意图解析的输入。
type IntentInput struct {
	Today     string // 如 "2026-11-24 星期二"，供模型理解相对时间（日期由本包换算）
	Projects  []ProjectInfo
	History   []HistoryItem
	Question  string
	NeedTitle bool // 需要同时生成会话标题
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
}
