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
}

// Tx 是事务内的工单操作。Lock* 以 SELECT … FOR UPDATE 锁定工单，同一工单的并发更新串行执行。
type Tx interface {
	// LockByIssue 按仓库（"owner/name"，不区分大小写）与主 Issue 编号查找并锁定工单；不存在返回 ErrNotFound。
	LockByIssue(ctx context.Context, repo string, number int64) (*Ticket, error)
	// LockByID 查找并锁定工单；不存在返回 ErrNotFound。
	LockByID(ctx context.Context, id int64) (*Ticket, error)
	// Update 保存 Status、IssueNumber、GiteaUpdatedAt、FixedPR、FixedCommit、Milestone、DueDate，并刷新 updated_at。
	Update(ctx context.Context, t *Ticket) error
	// AddHistory 写一条 ticket_events 审计记录。
	AddHistory(ctx context.Context, h History) error
	// SaveGiteaComment 按 GiteaID 新增或更新来自 Gitea 的评论，返回更新前的记录（新增时为 nil）。
	SaveGiteaComment(ctx context.Context, c Comment) (prev *Comment, err error)
	// DeleteGiteaComment 软删除来自 Gitea 的评论；评论不存在时不报错。
	DeleteGiteaComment(ctx context.Context, giteaID int64, at time.Time) error
	// Emit 写入对外事件（分配 ID），并在启用通知时写入通知 outbox。
	Emit(ctx context.Context, e notify.Event) error
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
	TicketID   int64
	GiteaID    int64
	AuthorName string
	Body       string
	Public     bool
	At         time.Time
}
