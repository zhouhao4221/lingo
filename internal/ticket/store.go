package ticket

import (
	"context"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
)

// Store 是工单仓储，由 internal/store 实现。
type Store interface {
	// InTx 在一个事务中执行 fn；fn 返回错误时回滚。
	InTx(ctx context.Context, fn func(Tx) error) error
	// HasIssue 报告仓库（"owner/name"，不区分大小写）的 Issue 是否是某个工单的主 Issue（不加锁）。
	HasIssue(ctx context.Context, repo string, number int64) (bool, error)
	// Get 读取工单（不加锁），含修复版本与上线时间；不存在返回 ErrNotFound。
	Get(ctx context.Context, id int64) (*Ticket, error)
	// GetMany 一次读取多张工单（不加锁），不存在的跳过，按 ID 升序。
	GetMany(ctx context.Context, ids []int64) ([]*Ticket, error)
}

// Tx 是事务内的工单操作。Lock* 以 SELECT … FOR UPDATE 锁定工单，同一工单的并发更新串行执行。
type Tx interface {
	// LockByIssue 按仓库（"owner/name"，不区分大小写）与主 Issue 编号查找并锁定工单；不存在返回 ErrNotFound。
	LockByIssue(ctx context.Context, repo string, number int64) (*Ticket, error)
	// LockByID 查找并锁定工单；不存在返回 ErrNotFound。
	LockByID(ctx context.Context, id int64) (*Ticket, error)
	// Update 保存可变字段（RepoID、Type、Title、Status、IssueNumber、MergedInto、TriageSince、GiteaUpdatedAt、
	// FixedPR、FixedCommit、Milestone、DueDate），并刷新 updated_at。
	Update(ctx context.Context, t *Ticket) error
	// Insert 新建工单，回填 ID、CreatedAt、UpdatedAt。
	Insert(ctx context.Context, t *Ticket) error
	// Project 按 key 查找已启用的项目；不存在返回 ErrNotFound。
	Project(ctx context.Context, key string) (*Project, error)
	// Repo 在项目内按 "owner/name"（不区分大小写）查找已启用的仓库；不属于该项目返回 ErrNotFound。
	Repo(ctx context.Context, projectID int64, name string) (*RepoRef, error)
	// AutoForward 返回项目 + 类型的运行时覆盖值；没有覆盖时 ok 为 false。
	AutoForward(ctx context.Context, projectID int64, typ string) (on, ok bool, err error)
	// SetAutoForward 写入（on 非空）或删除（on 为空）运行时覆盖值。
	SetAutoForward(ctx context.Context, projectID int64, typ string, on *bool, by string) error
	// LockOverdue 锁定并返回最多 limit 张在 before 之前进入待确认（或上次提醒早于 before）的工单，跳过已被锁定的。
	LockOverdue(ctx context.Context, s Scope, before time.Time, limit int) ([]*Ticket, error)
	// MarkReminded 记录提醒时间。
	MarkReminded(ctx context.Context, id int64, at time.Time) error
	// AddComment 写一条工单评论（用户补充或后台答复），返回评论 ID。
	AddComment(ctx context.Context, c Comment) (int64, error)
	// MarkCommentsForwarded 标记工单的补充评论已随 Issue 正文转出。
	MarkCommentsForwarded(ctx context.Context, ticketID int64, ids []int64) error
	// AttachmentIDs 返回工单（含补充评论）已有附件的 ID。
	AttachmentIDs(ctx context.Context, ticketID int64) ([]string, error)
	// Enqueue 写一条 Gitea 写操作任务；Key 已存在时忽略（同一操作只入队一次）。
	Enqueue(ctx context.Context, t Task) error
	// RepointMerged 把合并到 from 的工单改为合并到 to。
	RepointMerged(ctx context.Context, from, to int64) error
	// PendingTask 报告工单是否有未完成的 op 任务。
	PendingTask(ctx context.Context, ticketID int64, op string) (bool, error)
	// SeenPull 报告工单是否已记录过 PR pull（"owner/name#N"）的 kind 变化（ticket_events）。
	SeenPull(ctx context.Context, ticketID int64, kind, pull string) (bool, error)
	// AddHistory 写一条 ticket_events 审计记录。
	AddHistory(ctx context.Context, h History) error
	// SaveGiteaComment 按 GiteaID 新增或更新来自 Gitea 的评论，返回更新前的记录（新增时为 nil）；saved 为假表示未写入
	// （灵工已记为删除，或 c.At 早于已保存版本的乱序旧编辑），此时 prev 为已保存的记录。
	SaveGiteaComment(ctx context.Context, c Comment) (prev *Comment, saved bool, err error)
	// DeleteGiteaComment 软删除来自 Gitea 的评论；评论不存在时不报错。
	DeleteGiteaComment(ctx context.Context, giteaID int64, at time.Time) error
	// AddPerspective 写一段需求视角补充，返回 ID。
	AddPerspective(ctx context.Context, p Perspective) (int64, error)
	// Perspectives 返回工单的视角补充（不含首段），按时间升序。
	Perspectives(ctx context.Context, ticketID int64) ([]Perspective, error)
	// Emit 写入对外事件（分配 ID），并在启用通知时写入通知 outbox。
	Emit(ctx context.Context, e notify.Event) error
	// RejectionNote 返回最近一次转研发后驳回的原因与驳回人姓名快照（审计 ticket.rejected 的 note / rejected_by）；
	// 没有时返回空串。
	RejectionNote(ctx context.Context, ticketID int64) (reason, by string, err error)
	// ClaimCommentSend 为补充评论写入发送租约（数据库时间 now() + hold）；评论不属于该工单或已被拦下时返回 false。
	// 调用方须已在同一事务中锁定工单。
	ClaimCommentSend(ctx context.Context, ticketID, commentID int64, hold time.Duration) (bool, error)
	// ReleaseCommentSend 清除补充评论的发送租约。
	ReleaseCommentSend(ctx context.Context, ticketID, commentID int64) error
	// CommentSending 报告工单是否有未过期的补充评论发送租约（按数据库时间）。
	CommentSending(ctx context.Context, ticketID int64) (bool, error)
	// MarkCommentWithheld 标记补充评论因工单已驳回而不再发到 Gitea；已标记过（或评论不属于该工单）时返回 false。
	MarkCommentWithheld(ctx context.Context, ticketID, commentID int64) (bool, error)
	// TaskState 按幂等键读取 Gitea 写操作任务的执行状态；不存在时返回 nil。
	TaskState(ctx context.Context, key string) (*TaskState, error)
	// AbnormalFrom 返回工单最近一次进入 abnormal 之前的状态，取自与状态变更同事务写入的历史：
	// payload.status = abnormal 且 prev_status 不是 abnormal 的最新一条（建单失败 issue.create_failed 视为从 submitted 进入）。
	// 没有这样的历史时返回空串。
	AbnormalFrom(ctx context.Context, ticketID int64) (Status, error)
}

// TaskState 是 Gitea 写操作任务（outbox）的执行状态。
type TaskState struct {
	Key       string
	Status    string // pending | done | failed
	Failures  int
	LastError string
	UpdatedAt time.Time
}

// History 是 ticket_events 中的一条审计记录。
type History struct {
	TicketID int64
	ActorID  string // 接入方用户 ID；来自 Gitea 时为空，触发人 login 记在 Payload
	Source   string // api | mcp | go | gitea | system
	Kind     string
	Payload  map[string]any
}

// Comment 是工单评论（ticket_comments）。
type Comment struct {
	ID         int64 // ticket_comments.id（读取时回填）
	TicketID   int64
	GiteaID    int64  // 来自 Gitea 的评论 ID
	Source     string // user | staff | gitea；SaveGiteaComment 固定为 gitea
	AuthorID   string // 接入方用户 ID；来自 Gitea 时为空
	AuthorName string
	Body       string
	Public     bool
	At         time.Time
	Forwarded  bool  // 已随 Issue 正文转出
	PostedID   int64 // 已由机器人发到 Gitea 的评论 ID
	// Attachments 是随评论提交的附件引用（写入时使用）
	Attachments []Attachment
}

// Project 是已同步入库的项目。
type Project struct {
	ID          int64
	Key         string
	DefaultRepo *RepoRef // 未设置时为空
}

// RepoRef 是项目下的仓库。
type RepoRef struct {
	ID   int64
	Name string // "owner/name"
}

// 任务操作。
const (
	OpCreateIssue   = "create_issue"
	OpCreateComment = "create_comment"
	OpRemoveLabel   = "remove_label"
	// OpSyncPerspectives 把需求视角补充同步到 Issue：重写正文区块、补打视角标签、发提示评论。
	OpSyncPerspectives = "sync_perspectives"
	// OpCloseIssue 在工单转研发后被驳回时关闭工单的主 Issue（只关闭这一个，不动其他 Issue 与 PR）。
	OpCloseIssue = "close_issue"
)

// Task 是 Gitea 写操作任务（outbox kind=gitea）。
type Task struct {
	Key      string // 幂等键，如 "create_issue:ticket:42"
	TicketID int64
	Op       string
	Payload  map[string]any
}
