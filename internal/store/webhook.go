package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/webhook"
)

var _ webhook.Store = (*Store)(nil)

// SaveDelivery 实现 webhook.Store：delivery_id 或 dedup_key 冲突时不写入，返回 inserted=false。
func (s *Store) SaveDelivery(ctx context.Context, r webhook.Record) (bool, error) {
	received := r.ReceivedAt
	if received.IsZero() {
		received = time.Now()
	}
	payload := r.Payload
	if payload == nil {
		payload = []byte{}
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO lingo_webhook_deliveries
		    (delivery_id, event, event_type, kind, dedup_key, repo, issue_number, from_bot, payload, parse_error, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		r.DeliveryID, r.Event, r.EventType, string(r.Kind), r.DedupKey, strings.ToLower(r.Repo),
		nullInt64(r.IssueNumber), r.FromBot, payload, r.ParseError, received,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: save delivery %s: %w", r.DeliveryID, err)
	}
	return true, nil
}

// RecordPing 实现 webhook.Store：记录仓库最近一次连通时间，只会向后更新。
func (s *Store) RecordPing(ctx context.Context, repo string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO lingo_webhook_pings (repo, last_ping_at) VALUES ($1, $2)
		ON CONFLICT (repo) DO UPDATE SET last_ping_at = GREATEST(lingo_webhook_pings.last_ping_at, EXCLUDED.last_ping_at)`,
		strings.ToLower(repo), at)
	if err != nil {
		return fmt.Errorf("store: record ping %s: %w", repo, err)
	}
	return nil
}

// LastPing 返回仓库最近一次连通时间；从未收到过时 ok 为 false。
func (s *Store) LastPing(ctx context.Context, repo string) (at time.Time, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT last_ping_at FROM lingo_webhook_pings WHERE repo = $1", strings.ToLower(repo)).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: last ping %s: %w", repo, err)
	}
	return at, true, nil
}

// Delivery 是 worker 领取到的一条待处理投递。
//
// Record.FromBot 是接收时的判定，机器人账号当时可能尚未获取；worker 必须用 webhook.Parse 按当前的
// 机器人账号重新解析 Payload，不能直接信任该字段。
type Delivery struct {
	ID int64
	webhook.Record
	Attempts int // 含本次领取；同时是本次租约的令牌，Complete / Retry / Release / Abandon 必须匹配
	Failures int // 此前失败的次数（推迟与释放不计）
}

// DefaultLease 是 ClaimDeliveries 的默认租约时长。
const DefaultLease = 5 * time.Minute

// ErrLeaseLost 表示租约已失效：投递或通知在租约过期后被其他 worker 重新领取（attempts 已变化）或已处理完成，
// 本次处理结果不再写入。
var ErrLeaseLost = errors.New("store: lease lost")

// ClaimDeliveries 领取最多 limit 条到期的未处理投递，租约 lease（≤0 时为 DefaultLease）内其他实例不会再领取。
// 同一 Issue 的投递按顺序处理：存在更早的未完成投递时，后面的投递暂不领取。结果按 ID 升序。
func (s *Store) ClaimDeliveries(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error) {
	if limit <= 0 {
		limit = 20
	}
	if lease <= 0 {
		lease = DefaultLease
	}
	rows, err := s.db.QueryContext(ctx, `
		UPDATE lingo_webhook_deliveries d
		SET locked_until = now() + make_interval(secs => $2), attempts = d.attempts + 1
		WHERE d.id IN (
		    SELECT c.id FROM lingo_webhook_deliveries c
		    WHERE c.processed_at IS NULL
		      AND c.next_attempt_at <= now()
		      AND (c.locked_until IS NULL OR c.locked_until < now())
		      AND NOT EXISTS (
		          SELECT 1 FROM lingo_webhook_deliveries p
		          WHERE p.processed_at IS NULL AND p.repo = c.repo AND p.issue_number = c.issue_number AND p.id < c.id)
		    ORDER BY c.id
		    LIMIT $1
		    FOR UPDATE SKIP LOCKED)
		RETURNING d.id, d.delivery_id, d.event, d.event_type, d.kind, d.dedup_key, d.repo, d.issue_number,
		          d.from_bot, d.payload, d.parse_error, d.received_at, d.attempts, d.failures`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var kind string
		var issue sql.NullInt64
		if err := rows.Scan(&d.ID, &d.DeliveryID, &d.Event, &d.EventType, &kind, &d.DedupKey, &d.Repo, &issue,
			&d.FromBot, &d.Payload, &d.ParseError, &d.ReceivedAt, &d.Attempts, &d.Failures); err != nil {
			return nil, err
		}
		d.Kind, d.IssueNumber = webhook.Kind(kind), issue.Int64
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Delivery) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// 收尾操作只在租约仍属于本次领取（attempts 未变且未处理完成）时生效，否则返回 ErrLeaseLost。
const leaseHeld = ` WHERE id = $1 AND attempts = $2 AND processed_at IS NULL`

// CompleteDelivery 标记投递处理完成。
func (s *Store) CompleteDelivery(ctx context.Context, d Delivery) error {
	return s.finishDelivery(ctx, d, `UPDATE lingo_webhook_deliveries SET processed_at = now(), locked_until = NULL, last_error = ''`+leaseHeld)
}

// RetryDelivery 记一次失败并保存原因，在 retryAt 之后允许重新领取。
func (s *Store) RetryDelivery(ctx context.Context, d Delivery, reason string, retryAt time.Time) error {
	return s.finishDelivery(ctx, d, `UPDATE lingo_webhook_deliveries SET locked_until = NULL, failures = failures + 1, last_error = $3, next_attempt_at = $4`+leaseHeld,
		reason, retryAt)
}

// ReleaseDelivery 释放租约，在 retryAt 之后允许重新领取，不计失败：用于推迟处理与退出时归还未开始的条目。
func (s *Store) ReleaseDelivery(ctx context.Context, d Delivery, reason string, retryAt time.Time) error {
	return s.finishDelivery(ctx, d, `UPDATE lingo_webhook_deliveries SET locked_until = NULL, last_error = $3, next_attempt_at = $4`+leaseHeld,
		reason, retryAt)
}

// HasPendingMerges 报告与仓库 repo（"owner/name"）同项目的仓库中，是否有在 before 之前收到、尚未处理且未失败过的
// PR 合并投递。只看未失败过的：反复失败的合并投递不应无限期拖住其他工单的关闭判定。repo 不属于任何项目时只看它自己。
func (s *Store) HasPendingMerges(ctx context.Context, repo string, before time.Time) (bool, error) {
	var ok bool
	// kind 用字面量而非参数，使泛化执行计划也能命中部分索引 webhook_deliveries_merge_pending_idx
	err := s.db.QueryRowContext(ctx, `
		WITH project_repos AS (
		    SELECT lower(r.owner || '/' || r.name) AS name FROM lingo_repos r
		    WHERE r.project_id = (SELECT project_id FROM lingo_repos WHERE lower(owner || '/' || name) = lower($1))
		    UNION SELECT lower($1))
		SELECT EXISTS (SELECT 1 FROM lingo_webhook_deliveries
		    WHERE processed_at IS NULL AND kind = 'pull.merged' AND failures = 0 AND received_at <= $2
		      AND repo IN (SELECT name FROM project_repos))`, repo, before).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: pending merges: %w", err)
	}
	return ok, nil
}

// AbandonDelivery 放弃投递：标记为已处理并保留失败原因，不再重试，也不再阻塞同一 Issue 的后续投递
// （由对账补齐其影响）。
func (s *Store) AbandonDelivery(ctx context.Context, d Delivery, reason string) error {
	return s.finishDelivery(ctx, d, `UPDATE lingo_webhook_deliveries SET processed_at = now(), locked_until = NULL, last_error = $3`+leaseHeld,
		reason)
}

func (s *Store) finishDelivery(ctx context.Context, d Delivery, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, append([]any{d.ID, d.Attempts}, args...)...)
	if err != nil {
		return fmt.Errorf("store: update delivery %d: %w", d.ID, err)
	}
	// 驱动无法报告影响行数时按成功处理，避免把已生效的更新误报为失败
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: delivery %d attempt %d", ErrLeaseLost, d.ID, d.Attempts)
	}
	return nil
}
