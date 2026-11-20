package ticket

import (
	"context"
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
	CloseDone    = "done"    // 已关闭（或 Issue 已是关闭 / 已删除状态）
	CloseFailed  = "failed"  // 超过重试上限或遇到不可重试的错误，可调用 RetryIssueClose 重新入队
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

func enqueueClose(ctx context.Context, tx Tx, t *Ticket, attempt int) error {
	return tx.Enqueue(ctx, Task{Key: closeKey(t.ID, attempt), TicketID: t.ID, Op: OpCloseIssue,
		Payload: map[string]any{"issue": t.IssueNumber, "repo": t.Repo}})
}

// RejectForwarded 驳回已转研发、尚未修复的工单（reviewer / admin），reason 必填且对提交人可见。
// 工单、评论与历史全部保留，状态改为 rejected；工单已关联主 Issue 时入队关闭该 Issue（持久化、可重试，
// 同一工单只入队一次），不动其他 Issue 与 PR。驳回之后 Gitea 侧的关闭、重开、删除等变化不再改变工单状态。
// 对已驳回的工单重复调用直接返回当前工单，不重复留痕、通知或入队；待确认工单请用 Reject。
func (s *Service) RejectForwarded(ctx context.Context, p auth.Principal, id int64, reason string) (*Ticket, error) {
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
		}
		prev := t.Status
		t.Status = StatusRejected
		if err := tx.Update(ctx, t); err != nil {
			return err
		}
		payload := map[string]any{"status": string(t.Status), "prev_status": string(prev), "note": reason}
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
