package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
)

// Notification 是 worker 领取到的一条待投递通知。
type Notification struct {
	OutboxID int64
	Attempts int // 含本次领取；同时是本次租约的令牌
	Failures int // 此前失败的次数（释放不计）
	Event    notify.Event
}

// ClaimNotifications 领取最多 limit 条到期的待投递通知，租约 lease（≤0 时为 DefaultLease）。
// 同一工单的通知按事件顺序投递：存在更早的未完成通知时，后面的暂不领取。结果按 outbox ID 升序。
func (s *Store) ClaimNotifications(ctx context.Context, limit int, lease time.Duration) ([]Notification, error) {
	if limit <= 0 {
		limit = 20
	}
	if lease <= 0 {
		lease = DefaultLease
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH claimed AS (
		    UPDATE outbox o
		    SET locked_until = now() + make_interval(secs => $2), attempts = o.attempts + 1, updated_at = now()
		    WHERE o.id IN (
		        SELECT c.id FROM outbox c JOIN events ce ON ce.id = c.event_id
		        WHERE c.kind = 'notify' AND c.status = 'pending'
		          AND c.next_attempt_at <= now()
		          AND (c.locked_until IS NULL OR c.locked_until < now())
		          AND NOT EXISTS (
		              SELECT 1 FROM outbox p JOIN events pe ON pe.id = p.event_id
		              WHERE p.kind = 'notify' AND p.status = 'pending' AND pe.ticket_id = ce.ticket_id AND p.id < c.id)
		        ORDER BY c.id
		        LIMIT $1
		        FOR UPDATE OF c SKIP LOCKED)
		    RETURNING o.id, o.attempts, o.failures, o.event_id)
		SELECT c.id, c.attempts, c.failures, e.id, e.kind, e.created_at, COALESCE(e.project_id, 0), COALESCE(p.key, ''),
		       COALESCE(e.ticket_id, 0), e.status, e.prev_status, e.action, e.user_id, e.actor_id, e.payload
		FROM claimed c JOIN events e ON e.id = c.event_id LEFT JOIN projects p ON p.id = e.project_id`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: claim notifications: %w", err)
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		var kind string
		var payload []byte
		e := &n.Event
		if err := rows.Scan(&n.OutboxID, &n.Attempts, &n.Failures, &e.ID, &kind, &e.At, &e.ProjectID, &e.ProjectKey, &e.TicketID,
			&e.Status, &e.PrevStatus, &e.Action, &e.UserID, &e.ActorID, &payload); err != nil {
			return nil, err
		}
		e.Kind = notify.Kind(kind)
		var p eventPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("store: event %d payload: %w", e.ID, err)
		}
		e.TicketType, e.Title, e.Body, e.URL = p.TicketType, p.Title, p.Body, p.URL
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Notification) int { return cmp.Compare(a.OutboxID, b.OutboxID) })
	return out, nil
}

const outboxLeaseHeld = ` WHERE id = $1 AND attempts = $2 AND status = 'pending'`

// CompleteNotification 标记通知已投递。
func (s *Store) CompleteNotification(ctx context.Context, n Notification) error {
	return s.finishOutbox(ctx, n, `UPDATE outbox SET status = 'done', locked_until = NULL, last_error = '', updated_at = now()`+outboxLeaseHeld)
}

// RetryNotification 记一次失败并保存原因，在 retryAt 之后允许重新领取。
func (s *Store) RetryNotification(ctx context.Context, n Notification, reason string, retryAt time.Time) error {
	return s.finishOutbox(ctx, n, `UPDATE outbox SET locked_until = NULL, failures = failures + 1, last_error = $3, next_attempt_at = $4, updated_at = now()`+outboxLeaseHeld,
		reason, retryAt)
}

// ReleaseNotification 释放租约，在 retryAt 之后允许重新领取，不计失败。
func (s *Store) ReleaseNotification(ctx context.Context, n Notification, reason string, retryAt time.Time) error {
	return s.finishOutbox(ctx, n, `UPDATE outbox SET locked_until = NULL, last_error = $3, next_attempt_at = $4, updated_at = now()`+outboxLeaseHeld,
		reason, retryAt)
}

// FailNotification 放弃投递（超过重试上限），保留失败原因；不再阻塞同一工单的后续通知。
func (s *Store) FailNotification(ctx context.Context, n Notification, reason string) error {
	return s.finishOutbox(ctx, n, `UPDATE outbox SET status = 'failed', locked_until = NULL, last_error = $3, updated_at = now()`+outboxLeaseHeld,
		reason)
}

func (s *Store) finishOutbox(ctx context.Context, n Notification, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, append([]any{n.OutboxID, n.Attempts}, args...)...)
	if err != nil {
		return fmt.Errorf("store: update outbox %d: %w", n.OutboxID, err)
	}
	if c, err := res.RowsAffected(); err == nil && c == 0 {
		return fmt.Errorf("%w: outbox %d attempt %d", ErrLeaseLost, n.OutboxID, n.Attempts)
	}
	return nil
}
