package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

var _ ticket.QueryStore = (*Store)(nil)

// eventSettle 见 Events。
const eventSettle = 30 * time.Second

// scopeSQL 返回限定项目范围的条件（引用 p.key），占位符从 $n 开始，占两个参数。
func scopeSQL(n int) string {
	return fmt.Sprintf("($%d OR p.key = ANY(($%d::text)::text[]))", n, n+1)
}

func scopeArgs(s ticket.Scope) []any { return []any{s.All, textArray(s.Projects)} }

func (s *Store) tickets(ctx context.Context, query string, args ...any) ([]*ticket.Ticket, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list tickets: %w", err)
	}
	defer rows.Close()
	var out []*ticket.Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTriage 实现 ticket.QueryStore。
func (s *Store) ListTriage(ctx context.Context, q ticket.TriageQuery) ([]*ticket.Ticket, error) {
	var before sql.NullTime
	if !q.PendingBefore.IsZero() {
		before = sql.NullTime{Time: q.PendingBefore, Valid: true}
	}
	return s.tickets(ctx, "SELECT "+ticketColumns+`
		WHERE t.status = 'pending_triage' AND p.enabled AND `+scopeSQL(1)+`
		  AND ($3 = '' OR t.type = $3) AND ($4::timestamptz IS NULL OR t.triage_since < $4) AND t.id > $5
		ORDER BY t.id LIMIT $6`, append(scopeArgs(q.Scope), q.Type, before, q.AfterID, q.Limit)...)
}

// TriageCounts 实现 ticket.QueryStore：范围内每个已启用项目一行（没有待确认的为 0）。
func (s *Store) TriageCounts(ctx context.Context, sc ticket.Scope, overdueBefore time.Time) ([]ticket.TriageCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.key, count(t.id), count(t.id) FILTER (WHERE t.triage_since < $3)
		FROM projects p LEFT JOIN tickets t ON t.project_id = p.id AND t.status = 'pending_triage'
		WHERE p.enabled AND `+scopeSQL(1)+`
		GROUP BY p.key ORDER BY p.key`, append(scopeArgs(sc), overdueBefore)...)
	if err != nil {
		return nil, fmt.Errorf("store: triage counts: %w", err)
	}
	defer rows.Close()
	var out []ticket.TriageCount
	for rows.Next() {
		var c ticket.TriageCount
		if err := rows.Scan(&c.ProjectKey, &c.Pending, &c.Overdue); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListMine 实现 ticket.QueryStore。
func (s *Store) ListMine(ctx context.Context, q ticket.MineQuery) ([]*ticket.Ticket, error) {
	return s.tickets(ctx, "SELECT "+ticketColumns+`
		WHERE t.user_id = $1 AND ($2 = '' OR t.status = $2) AND ($3::bigint = 0 OR t.id < $3::bigint)
		ORDER BY t.id DESC LIMIT $4`, q.UserID, q.Status, q.BeforeID, q.Limit)
}

// Events 实现 ticket.QueryStore。事件 ID 在事务内分配、提交顺序可能不同，直接按 ID 翻页时游标可能越过一个
// 尚未提交的较小 ID 而永远漏掉它，所以只返回早于所有进行中事务写入的事件（txid < 快照 xmin）。xmin 是整个实例
// 最老的进行中事务，宿主的长事务会把它拖住；因此写入超过 eventSettle 的事件无论如何都返回（灵问自己的事务
// 都是毫秒级，不会在这么久后才提交），变更流的延迟最多为 eventSettle。
func (s *Store) Events(ctx context.Context, q ticket.EventQuery) ([]notify.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.kind, e.created_at, COALESCE(e.project_id, 0), COALESCE(p.key, ''), COALESCE(e.ticket_id, 0),
		       e.status, e.prev_status, e.action, e.user_id, e.actor_id, e.payload
		FROM events e LEFT JOIN projects p ON p.id = e.project_id
		WHERE e.id > $3 AND (e.txid < pg_snapshot_xmin(pg_current_snapshot()) OR e.created_at < now() - $8 * interval '1 second')
		  AND `+scopeSQL(1)+`
		  AND ($4 = '' OR e.user_id = $4)
		  AND (NOT $5 OR e.kind NOT LIKE 'triage.%')
		  AND (NOT $6 OR e.status NOT IN ('pending_triage', 'answered', 'merged', 'rejected'))
		ORDER BY e.id LIMIT $7`, append(scopeArgs(q.Scope), q.AfterID, q.UserID, q.ExcludeTriage, q.ForwardedOnly, q.Limit,
		eventSettle.Seconds())...)
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	defer rows.Close()
	var out []notify.Event
	for rows.Next() {
		var e notify.Event
		var kind string
		var payload []byte
		if err := rows.Scan(&e.ID, &kind, &e.At, &e.ProjectID, &e.ProjectKey, &e.TicketID, &e.Status, &e.PrevStatus,
			&e.Action, &e.UserID, &e.ActorID, &payload); err != nil {
			return nil, err
		}
		e.Kind = notify.Kind(kind)
		var p eventPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("store: event %d payload: %w", e.ID, err)
		}
		e.TicketType, e.Title, e.Body, e.URL = p.TicketType, p.Title, p.Body, p.URL
		out = append(out, e)
	}
	return out, rows.Err()
}

// Comments 实现 ticket.QueryStore：未删除的评论，按时间顺序。
func (s *Store) Comments(ctx context.Context, ticketID int64, publicOnly bool) ([]ticket.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ticket_id, COALESCE(gitea_comment_id, 0), source, author_id, author_name, body, public, created_at
		FROM ticket_comments WHERE ticket_id = $1 AND deleted_at IS NULL AND (NOT $2 OR public)
		ORDER BY created_at, id`, ticketID, publicOnly)
	if err != nil {
		return nil, fmt.Errorf("store: comments %d: %w", ticketID, err)
	}
	defer rows.Close()
	var out []ticket.Comment
	for rows.Next() {
		var c ticket.Comment
		if err := rows.Scan(&c.ID, &c.TicketID, &c.GiteaID, &c.Source, &c.AuthorID, &c.AuthorName, &c.Body, &c.Public, &c.At); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AutoForwardOverrides 实现 ticket.QueryStore：返回 project key → 类型 → 覆盖值，一次查询。
func (s *Store) AutoForwardOverrides(ctx context.Context, keys []string) (map[string]map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.key, ps.ticket_type, ps.auto_forward FROM project_settings ps
		JOIN projects p ON p.id = ps.project_id WHERE p.key = ANY(($1::text)::text[])`, textArray(keys))
	if err != nil {
		return nil, fmt.Errorf("store: auto forward overrides: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var key, typ string
		var on bool
		if err := rows.Scan(&key, &typ, &on); err != nil {
			return nil, err
		}
		if out[key] == nil {
			out[key] = map[string]bool{}
		}
		out[key][typ] = on
	}
	return out, rows.Err()
}

func (x *ticketTx) LockOverdue(ctx context.Context, s ticket.Scope, before time.Time, limit int) ([]*ticket.Ticket, error) {
	rows, err := x.tx.QueryContext(ctx, "SELECT "+ticketColumns+`
		WHERE t.status = 'pending_triage' AND COALESCE(t.triage_reminded_at, t.triage_since) < $1
		  AND p.enabled AND `+scopeSQL(3)+`
		ORDER BY t.id LIMIT $2 FOR UPDATE OF t SKIP LOCKED`, append([]any{before, limit}, scopeArgs(s)...)...)
	if err != nil {
		return nil, fmt.Errorf("store: lock overdue: %w", err)
	}
	defer rows.Close()
	var out []*ticket.Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (x *ticketTx) MarkReminded(ctx context.Context, id int64, at time.Time) error {
	if _, err := x.tx.ExecContext(ctx, "UPDATE tickets SET triage_reminded_at = $2 WHERE id = $1", id, at); err != nil {
		return fmt.Errorf("store: mark reminded %d: %w", id, err)
	}
	return nil
}
