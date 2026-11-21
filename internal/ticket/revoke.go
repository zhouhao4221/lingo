package ticket

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/notify"
)

// 关闭关联 Issue 任务的状态（IssueClose.State）。
const (
	CloseNone    = "none"    // 没有要关闭的 Issue（驳回时 Issue 尚未创建，或工单不是转研发后驳回的）
	ClosePending = "pending" // 排队或重试中
	CloseDone    = "done"    // 已关闭，或 Issue 已是关闭状态
	CloseFailed  = "failed"  // 超过重试上限或遇到不可重试的错误（含 Issue 返回 404、无法确认已删除），可调用 RetryIssueClose 重新入队
)

// IssueClose 是驳回后关闭关联 Issue 的进度。
type IssueClose struct {
	TicketID  int64
	Repo      string // "owner/name"；没有关联 Issue 时为空
	Number    int64  // 关联的主 Issue 编号；没有时为 0
	State     string // none | pending | done | failed
	Attempt   int    // 第几轮关闭任务（首轮为 1，每次 RetryIssueClose 加 1）；没有任务为 0
	Failures  int    // 本轮失败次数
	LastError string // 本轮最近一次失败原因
	UpdatedAt *time.Time
}

// closeKey 是第 attempt 轮关闭任务的幂等键：首轮 close_issue:ticket:<id>，重试轮次追加 :retry:<n>。
// 同一轮只入队一次；读取进度时按轮次依次查找，不需要额外的索引或表。
func closeKey(id int64, attempt int) string {
	key := OpCloseIssue + ":ticket:" + strconv.FormatInt(id, 10)
	if attempt > 1 {
		key += ":retry:" + strconv.Itoa(attempt-1)
	}
	return key
}

// hasFix 报告工单是否已记录修复（PR、提交、修复版本或上线）。
func hasFix(t *Ticket) bool {
	return t.FixedPR != 0 || t.FixedCommit != "" || t.FixedReleaseID != 0 || t.DeployedAt != nil
}

// enqueueClose 入队第 attempt 轮关闭任务。驳回原因与驳回人姓名取自审计 ticket.rejected（与驳回同事务写入），
// 由 worker 写成 Issue 上的一条原因评论：重试轮次、建单途中驳回后补记 Issue 时都沿用原驳回原因与署名。
func enqueueClose(ctx context.Context, tx Tx, t *Ticket, attempt int) error {
	reason, by, err := tx.RejectionNote(ctx, t.ID)
	if err != nil {
		return err
	}
	return tx.Enqueue(ctx, Task{Key: closeKey(t.ID, attempt), TicketID: t.ID, Op: OpCloseIssue,
		Payload: map[string]any{"issue": t.IssueNumber, "repo": t.Repo, "reason": reason, "rejected_by": by}})
}

// RejectForwarded 驳回已转研发、尚未修复的工单（reviewer / admin），reason 必填且对提交人可见。
// abnormal 工单只在没有修复记录、且不是从 closed 进入时可驳回（建单失败，或 Issue 被删除时尚在处理中；已关闭工单的
// Issue 被删除时保持 closed，早期版本已由 closed 转入的按状态历史识别）；已记录修复 PR / 提交 / 版本 / 上线的返回 ErrConflict。
// 工单、评论与历史全部保留，状态改为 rejected；工单已关联主 Issue 时入队关闭该 Issue（持久化、可重试，
// 同一工单只入队一次），不动其他 Issue 与 PR。驳回之后 Gitea 侧的关闭、重开、删除等变化不再改变工单状态。
// 对已驳回的工单重复调用直接返回当前工单，不重复留痕、通知或入队；待确认工单请用 Reject。
//
// 与补充评论外发仲裁（见 ClaimCommentSend）：工单有补充评论正在发往 Gitea（发送租约有效）时本次不提交，在事务外每 100 毫秒
// 重试，最多等 SendWait（默认 5 秒），仍在发送则返回 ErrConflict（稍后重试即可）。
func (s *Service) RejectForwarded(ctx context.Context, p auth.Principal, id int64, reason string) (*Ticket, error) {
	deadline := time.Now().Add(s.o.SendWait)
	for {
		t, err := s.rejectForwarded(ctx, p, id, reason)
		if !errors.Is(err, errSending) {
			return t, err
		}
		wait := min(rejectPoll, time.Until(deadline))
		if wait <= 0 {
			return nil, fmt.Errorf("%w: ticket %d has a comment being sent to gitea, retry later", ErrConflict, id)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// errSending 表示工单有补充评论正在发往 Gitea，驳回事务已回滚，可稍后重试。
var errSending = errors.New("ticket: comment being sent")

// rejectPoll 是驳回等待评论发送结束时的重试间隔。
const rejectPoll = 100 * time.Millisecond

func (s *Service) rejectForwarded(ctx context.Context, p auth.Principal, id int64, reason string) (*Ticket, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: rejecting a forwarded ticket requires reviewer or admin", ErrForbidden)
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrInvalid)
	}
	var t *Ticket
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		var err error
		if t, err = tx.LockByID(ctx, id); err != nil {
			return err
		}
		if !p.InProject(t.ProjectKey) {
			return ErrNotFound
		}
		switch {
		case t.Status == StatusRejected:
			return nil // 幂等：已驳回
		case t.Status == StatusPendingTriage:
			return fmt.Errorf("%w: ticket %d is pending_triage, use reject", ErrConflict, id)
		case !t.Status.revocable():
			return fmt.Errorf("%w: ticket %d is %s, cannot be rejected", ErrConflict, id, t.Status)
		case hasFix(t):
			// 已修复的工单 Issue 被删除后为 abnormal，修复记录仍在：同样不可驳回
			return fmt.Errorf("%w: ticket %d has a recorded fix, cannot be rejected", ErrConflict, id)
		}
		if t.Status == StatusAbnormal {
			// 早期版本会把已关闭工单在 Issue 删除（或读取 404）后改为 abnormal：按同事务写入的状态历史确定进入 abnormal
			// 之前的状态（行锁内读取），从 closed 进入的不可驳回
			from, err := tx.AbnormalFrom(ctx, t.ID)
			if err != nil {
				return err
			}
			if from == StatusClosed {
				return fmt.Errorf("%w: ticket %d was closed before it became abnormal, cannot be rejected", ErrConflict, id)
			}
		}
		// 仲裁：工单已锁定，补充评论的发送租约只能在这把锁下写入；有有效租约说明发送先取得仲裁，本次不提交
		if sending, err := tx.CommentSending(ctx, t.ID); err != nil {
			return err
		} else if sending {
			return errSending
		}
		prev := t.Status
		t.Status = StatusRejected
		if err := tx.Update(ctx, t); err != nil {
			return err
		}
		// rejected_by 是驳回人姓名快照，用于 Issue 原因评论署名（未提供姓名时不署名，不写用户 ID）
		payload := map[string]any{"status": string(t.Status), "prev_status": string(prev), "note": reason,
			"rejected_by": strings.TrimSpace(p.Name)}
		if t.IssueNumber != 0 {
			payload["issue"], payload["repo"] = t.IssueNumber, t.Repo
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, ActorID: p.ID, Source: sourceOf(ctx), Kind: "ticket.rejected",
			Payload: payload}); err != nil {
			return err
		}
		if err := tx.Emit(ctx, s.event(t, notify.TicketStatus, prev, p.ID, reason)); err != nil {
			return err
		}
		if t.IssueNumber == 0 {
			return nil // Issue 尚未创建：建单任务会跳过；若 Gitea 已建好而结果未记下，IssueCreated 补记后再关闭
		}
		return enqueueClose(ctx, tx, t, 1)
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// IssueClose 返回工单驳回后关闭关联 Issue 的进度（reviewer / admin）。
func (s *Service) IssueClose(ctx context.Context, p auth.Principal, id int64) (*IssueClose, error) {
	return s.issueClose(ctx, p, id, false)
}

// RetryIssueClose 在关闭关联 Issue 的任务失败后重新入队（reviewer / admin），返回入队后的进度。
// 任务仍在排队或已完成时不重复入队，直接返回当前进度；没有可关闭的 Issue 时返回 ErrConflict。
func (s *Service) RetryIssueClose(ctx context.Context, p auth.Principal, id int64) (*IssueClose, error) {
	return s.issueClose(ctx, p, id, true)
}

func (s *Service) issueClose(ctx context.Context, p auth.Principal, id int64, retry bool) (*IssueClose, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: issue close status requires reviewer or admin", ErrForbidden)
	}
	var out *IssueClose
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByID(ctx, id) // 加锁：并发重试串行执行，只入队一轮
		if err != nil {
			return err
		}
		if !p.InProject(t.ProjectKey) {
			return ErrNotFound
		}
		out = &IssueClose{TicketID: t.ID, State: CloseNone}
		if t.Status == StatusRejected && t.IssueNumber != 0 {
			out.Repo, out.Number = t.Repo, t.IssueNumber
		}
		attempt, st, err := latestClose(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if st != nil {
			out.Attempt, out.State, out.Failures, out.LastError = attempt, st.Status, st.Failures, st.LastError
			at := st.UpdatedAt
			out.UpdatedAt = &at
		}
		if !retry {
			return nil
		}
		if out.Number == 0 {
			return fmt.Errorf("%w: ticket %d has no issue to close", ErrConflict, id)
		}
		if st != nil && st.Status != CloseFailed {
			return nil // 排队中或已完成：不重复入队
		}
		attempt++
		if err := enqueueClose(ctx, tx, t, attempt); err != nil {
			return err
		}
		if err := tx.AddHistory(ctx, History{TicketID: t.ID, ActorID: p.ID, Source: sourceOf(ctx), Kind: "issue.close_retry",
			Payload: map[string]any{"issue": t.IssueNumber, "repo": t.Repo, "attempt": attempt}}); err != nil {
			return err
		}
		out.Attempt, out.State, out.Failures, out.LastError, out.UpdatedAt = attempt, ClosePending, 0, "", nil
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// latestClose 返回最近一轮关闭任务的轮次与状态；没有任务时返回 0, nil。
func latestClose(ctx context.Context, tx Tx, id int64) (int, *TaskState, error) {
	var last *TaskState
	n := 0
	for {
		st, err := tx.TaskState(ctx, closeKey(id, n+1))
		if err != nil {
			return 0, nil, err
		}
		if st == nil {
			return n, last, nil
		}
		n, last = n+1, st
	}
}

// ClaimCommentSend 是补充评论外发与驳回的仲裁点：在锁定工单的短事务中判定，返回 true 表示 worker 取得发送租约，
// 可以在 hold 内发出这条评论及其附件（完成后调用 ReleaseCommentSend）；返回 false 表示不发。
//   - 工单已驳回（驳回先提交）：不发，评论留在灵问，置 withheld_at 并记审计 comment.withheld（source=system，同一条只记一次，不发事件）；
//   - 否则写入租约（数据库时间 now() + hold）后提交：此后驳回在同一把工单锁下看到有效租约，不会在本次发送结束（或租约过期）前提交。
//
// 租约只在事务内写入，HTTP 请求在事务之外发出，不持有数据库连接与行锁。
func (s *Service) ClaimCommentSend(ctx context.Context, ticketID, commentID int64, hold time.Duration) (bool, error) {
	ok := false
	err := s.o.Store.InTx(ctx, func(tx Tx) error {
		t, err := tx.LockByID(ctx, ticketID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if t.Status == StatusRejected {
			marked, err := tx.MarkCommentWithheld(ctx, ticketID, commentID)
			if err != nil || !marked {
				return err
			}
			return tx.AddHistory(ctx, History{TicketID: t.ID, Source: "system", Kind: "comment.withheld",
				Payload: map[string]any{"comment": commentID, "issue": t.IssueNumber, "repo": t.Repo}})
		}
		ok, err = tx.ClaimCommentSend(ctx, ticketID, commentID, hold)
		return err
	})
	return ok && err == nil, err
}

// ReleaseCommentSend 在本次发送（评论及其附件）结束后清除租约，驳回可以立即提交；释放失败时租约到期自然失效。
func (s *Service) ReleaseCommentSend(ctx context.Context, ticketID, commentID int64) error {
	return s.o.Store.InTx(ctx, func(tx Tx) error { return tx.ReleaseCommentSend(ctx, ticketID, commentID) })
}
