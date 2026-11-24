package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// executor 在事务中执行一种待确认操作；返回的错误为 ticket.ErrNotFound / ErrConflict / ErrInvalid / ErrForbidden 时
// 视为被拒（操作置为 failed），其他错误视为临时错误（操作保持 pending，可再次确认）。
type executor func(ctx context.Context, tx Tx, p auth.Principal, a *Action) (*execResult, error)

// execResult 是操作执行的结果。
type execResult struct {
	Result  any    // 存入操作的 result
	Content string // 追加到会话的结果消息
	Cites   Citations
	Comment *ticket.Comment
}

// ActionResult 是确认或取消待确认操作的结果。
type ActionResult struct {
	Action  *Action
	Message *Message        // 追加到会话的结果消息；幂等返回时为空
	Comment *ticket.Comment // ticket_comment 执行成功时
}

func rejected(err error) bool {
	return errors.Is(err, ticket.ErrNotFound) || errors.Is(err, ticket.ErrConflict) || errors.Is(err, ticket.ErrInvalid) ||
		errors.Is(err, ticket.ErrForbidden)
}

func (s *Service) execComment(ctx context.Context, tx Tx, p auth.Principal, a *Action) (*execResult, error) {
	var pl CommentPayload
	if err := json.Unmarshal(a.Payload, &pl); err != nil || pl.TicketID <= 0 {
		return nil, fmt.Errorf("%w: bad payload", ticket.ErrInvalid)
	}
	c, err := s.o.Tickets.AddCommentTx(ctx, tx, p, pl.TicketID, pl.Body, nil) // 再次鉴权，审计、同步与通知同 AddComment
	if err != nil {
		return nil, err
	}
	t, err := tx.LockByID(ctx, pl.TicketID) // 已在 AddCommentTx 中锁定
	if err != nil {
		return nil, err
	}
	return &execResult{
		Result:  map[string]any{"comment_id": c.ID, "ticket_id": t.ID},
		Content: fmt.Sprintf("已在 #%d「%s」留言。", t.ID, oneLine(t.Title)),
		Cites:   Citations{Tickets: []TicketRef{s.ticketRef(t)}},
		Comment: c,
	}, nil
}

// rejectText 是操作被拒时给用户的说明（不区分无权与不存在）。
func rejectText(a *Action, err error) string {
	var pl CommentPayload
	_ = json.Unmarshal(a.Payload, &pl)
	switch {
	case errors.Is(err, ticket.ErrConflict):
		return fmt.Sprintf("没有留言：工单 #%d 当前状态不允许留言（如已合并到其他工单，或尚未建立 Issue）。", pl.TicketID)
	case errors.Is(err, ticket.ErrInvalid):
		return "没有留言：留言内容不合法。"
	default:
		return fmt.Sprintf("没有留言：没找到工单 #%d，或你没有权限在该工单留言。", pl.TicketID)
	}
}

// executeTx 在事务中锁定并执行操作 id，结果写入 msg（未落库）。执行器在保存点内运行：被拒（无权限、工单状态
// 不允许）时回滚到保存点，在同一事务中把操作置为 failed 并写入说明，不需要另开事务（期间租约不会被别人取得）。
// 操作已不在等待中（已取消、已过期、已失败）时返回 ticket.ErrConflict 与该操作。
func (s *Service) executeTx(ctx context.Context, tx Tx, p auth.Principal, convID, id int64, msg *Message) (*Action, *execResult, error) {
	a, err := tx.LockAction(ctx, convID, id)
	if err != nil {
		return nil, nil, err
	}
	now := s.o.Now()
	switch {
	case a.Status == ActionExecuted:
		return a, nil, nil // 已执行：幂等
	case a.Status != ActionPending:
		return a, nil, fmt.Errorf("%w: action is %s", ticket.ErrConflict, a.Status)
	case !now.Before(a.ExpiresAt):
		a.Status, a.ResolvedAt = ActionExpired, &now
		if err := tx.ResolveAction(ctx, a); err != nil {
			return nil, nil, err
		}
		msg.Content, msg.Outcome = textExpired, OutcomeActionExpired
		return a, nil, nil
	}
	var res *execResult
	if exec := s.execs[a.Kind]; exec == nil {
		err = fmt.Errorf("%w: unknown action kind %q", ticket.ErrInvalid, a.Kind)
	} else {
		if err := tx.Savepoint(ctx, "assistant_exec"); err != nil {
			return nil, nil, err
		}
		if res, err = exec(ctx, tx, p, a); err != nil && rejected(err) {
			if rerr := tx.RollbackToSavepoint(ctx, "assistant_exec"); rerr != nil {
				return nil, nil, rerr
			}
		}
	}
	switch {
	case err != nil && rejected(err):
		reason := strings.TrimPrefix(err.Error(), "ticket: ")
		if errors.Is(err, ticket.ErrNotFound) {
			reason = "not found" // 不区分无权与不存在
		}
		a.Status, a.ResolvedAt = ActionFailed, &now
		a.Result, _ = json.Marshal(map[string]string{"error": reason})
		if err := tx.ResolveAction(ctx, a); err != nil {
			return nil, nil, err
		}
		msg.Content, msg.Outcome = rejectText(a, err), OutcomeActionFailed
		return a, nil, nil
	case err != nil:
		return nil, nil, err // 临时错误：整个事务回滚，操作保持 pending
	}
	if a.Result, err = json.Marshal(res.Result); err != nil {
		return nil, nil, err
	}
	a.Status, a.ResolvedAt = ActionExecuted, &now
	if err := tx.ResolveAction(ctx, a); err != nil {
		return nil, nil, err
	}
	msg.Content, msg.Outcome, msg.Citations = res.Content, OutcomeActionExecuted, res.Cites
	return a, res, nil
}

// Confirm 确认执行待确认操作（确认按钮）：成功时追加结果消息；对已执行的操作再次确认返回原结果，不重复执行；
// 已取消、过期或失败的返回 ticket.ErrConflict。操作被拒（无权限、工单状态不允许）时置为 failed 并追加说明。
func (s *Service) Confirm(ctx context.Context, p auth.Principal, convID, actionID int64) (*ActionResult, error) {
	if err := caller(p); err != nil {
		return nil, err
	}
	msg := &Message{ConversationID: convID, Role: RoleAssistant, Status: StatusDone, Intent: IntentConfirm}
	var out *ActionResult
	var expired bool
	err := s.buttonTx(ctx, p, convID, func(tx Tx) error {
		a, res, err := s.executeTx(ctx, tx, p, convID, actionID, msg)
		if err != nil {
			return err
		}
		if res == nil && msg.Outcome == "" { // 已执行：幂等返回
			out = &ActionResult{Action: a}
			return nil
		}
		if expired = msg.Outcome == OutcomeActionExpired; expired {
			out = &ActionResult{Action: a} // 过期：只落库状态，不追加消息
			return nil
		}
		if err := s.appendMessage(ctx, tx, msg); err != nil {
			return err
		}
		out = &ActionResult{Action: a, Message: msg}
		if res != nil {
			out.Comment = res.Comment
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if expired {
		return out, fmt.Errorf("%w: action expired", ticket.ErrConflict)
	}
	return out, nil
}

// Cancel 取消待确认操作（取消按钮）：追加说明消息；对已取消的操作幂等返回，已执行、过期或失败的返回 ticket.ErrConflict。
func (s *Service) Cancel(ctx context.Context, p auth.Principal, convID, actionID int64) (*ActionResult, error) {
	if err := caller(p); err != nil {
		return nil, err
	}
	var out *ActionResult
	err := s.buttonTx(ctx, p, convID, func(tx Tx) error {
		a, err := tx.LockAction(ctx, convID, actionID)
		if err != nil {
			return err
		}
		now := s.o.Now()
		switch {
		case a.Status == ActionCancelled:
			out = &ActionResult{Action: a}
			return nil
		case a.Status != ActionPending:
			return fmt.Errorf("%w: action is %s", ticket.ErrConflict, a.Status)
		case !now.Before(a.ExpiresAt):
			a.Status, a.ResolvedAt = ActionExpired, &now
			if err := tx.ResolveAction(ctx, a); err != nil {
				return err
			}
			out = &ActionResult{Action: a}
			return nil
		}
		a.Status, a.ResolvedAt = ActionCancelled, &now
		if err := tx.ResolveAction(ctx, a); err != nil {
			return err
		}
		msg := &Message{ConversationID: convID, Role: RoleAssistant, Status: StatusDone, Intent: IntentCancel,
			Content: textCancelled, Outcome: OutcomeActionCancelled}
		if err := s.appendMessage(ctx, tx, msg); err != nil {
			return err
		}
		out = &ActionResult{Action: a, Message: msg}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out.Action.Status == ActionExpired {
		return out, fmt.Errorf("%w: action expired", ticket.ErrConflict)
	}
	return out, nil
}

// buttonTx 在事务中执行确认 / 取消按钮：与一轮提问互斥（取得生成租约，提问还在处理时返回 ticket.ErrConflict），
// 避免按钮插入的消息与正在生成的回答交错；租约在同一事务内释放。他人的会话返回 ticket.ErrNotFound。
func (s *Service) buttonTx(ctx context.Context, p auth.Principal, convID int64, fn func(Tx) error) error {
	return s.o.Store.InAssistantTx(ctx, func(tx Tx) error {
		conv, err := s.claim(ctx, tx, p, convID)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return tx.ReleaseTurn(ctx, convID, conv.Lease)
	})
}

// notPendingText 是执行时发现操作已不在等待中（如同时点了取消按钮）的说明。
func notPendingText(a *Action) string {
	if a == nil {
		return textNoAction
	}
	switch a.Status {
	case ActionExecuted:
		return "这个操作已经执行过了。"
	case ActionCancelled:
		return "这个操作已经取消，没有执行。"
	case ActionExpired:
		return textExpired
	}
	return "这个操作没有执行成功，需要的话请重新发起。"
}

// appendMessage 写入助手消息并刷新会话的最近活动。
func (s *Service) appendMessage(ctx context.Context, tx Tx, m *Message) error {
	if err := tx.InsertMessage(ctx, m); err != nil {
		return err
	}
	return tx.Touch(ctx, m.ConversationID, "")
}
