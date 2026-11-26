package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
		FROM lingo_projects p LEFT JOIN lingo_tickets t ON t.project_id = p.id AND t.status = 'pending_triage'
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

// ListVisible 实现 ticket.QueryStore。
func (s *Store) ListVisible(ctx context.Context, q ticket.VisibleQuery) ([]*ticket.Ticket, error) {
	args := scopeArgs(q.Scope)
	n := len(args)
	args = append(args, textArray(q.UserIDs), q.Status, q.BeforeID, q.Limit)
	return s.tickets(ctx, "SELECT "+ticketColumns+fmt.Sprintf(`
		WHERE %s AND t.user_id = ANY(($%d::text)::text[]) AND ($%d = '' OR t.status = $%d) AND ($%d::bigint = 0 OR t.id < $%d::bigint)
		ORDER BY t.id DESC LIMIT $%d`, scopeSQL(1), n+1, n+2, n+2, n+3, n+3, n+4), args...)
}

// forwardedAtSQL 是工单的转出时间：后台确认转出（triage.forward，含异常工单重新转出时取最早一次）的时间；
// 没有确认记录的已转出工单为自动转出，取提交时间。未转出的为 NULL。
const forwardedAtSQL = `(CASE WHEN t.status IN ('pending_triage', 'answered', 'merged', 'rejected') THEN NULL ELSE
	COALESCE((SELECT min(h.created_at) FROM lingo_ticket_events h WHERE h.ticket_id = t.id AND h.kind = 'triage.forward'), t.created_at) END)`

// searchWhere 返回 SearchQuery 的条件（不含 ORDER / LIMIT）与参数。
func searchWhere(q ticket.SearchQuery) (string, []any) {
	args := scopeArgs(q.Scope)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	w := []string{scopeSQL(1)}
	if q.UserID != "" {
		w = append(w, "t.user_id = "+arg(q.UserID))
	}
	if len(q.UserIDs) > 0 {
		w = append(w, "t.user_id = ANY(("+arg(textArray(q.UserIDs))+"::text)::text[])")
	}
	if q.ForwardedOnly {
		w = append(w, "t.status NOT IN ('pending_triage', 'answered', 'merged', 'rejected')")
	}
	if len(q.IDs) > 0 {
		w = append(w, "t.id = ANY(("+arg(textArrayInt(q.IDs))+"::text)::bigint[])")
	}
	if q.IssueNumber > 0 {
		w = append(w, "t.gitea_issue_number = "+arg(q.IssueNumber))
	}
	if q.Repo != "" {
		owner, name, _ := strings.Cut(q.Repo, "/")
		w = append(w, "lower(r.owner) = lower("+arg(owner)+") AND lower(r.name) = lower("+arg(name)+")")
	}
	if q.Type != "" {
		w = append(w, "t.type = "+arg(q.Type))
	}
	if len(q.Statuses) > 0 {
		w = append(w, "t.status = ANY(("+arg(textArray(q.Statuses))+"::text)::text[])")
	}
	if q.SubmitterName != "" {
		w = append(w, "t.user_name ILIKE "+arg("%"+likeEscape(q.SubmitterName)+"%")+" ESCAPE '\\'")
	}
	if q.Order == ticket.OrderTriageAsc { // 只含当前待确认的（triage_since 在离开待确认时清空）
		w = append(w, "t.status = 'pending_triage' AND t.triage_since IS NOT NULL")
	}
	for _, k := range q.Keywords {
		p := arg("%" + likeEscape(k) + "%")
		w = append(w, "(t.title ILIKE "+p+" ESCAPE '\\' OR t.body ILIKE "+p+" ESCAPE '\\')")
	}
	col := "t.created_at"
	switch q.TimeField {
	case ticket.TimeUpdated:
		col = "t.updated_at"
	case ticket.TimeForwarded:
		col = forwardedAtSQL
	}
	if !q.From.IsZero() {
		w = append(w, col+" >= "+arg(q.From))
	}
	if !q.To.IsZero() {
		w = append(w, col+" < "+arg(q.To))
	}
	if q.TimeField == ticket.TimeForwarded && q.From.IsZero() && q.To.IsZero() {
		w = append(w, col+" IS NOT NULL")
	}
	return strings.Join(w, " AND "), args
}

// likeEscape 转义 LIKE 模式中的反斜杠、% 与 _（配合 ESCAPE 子句）。
func likeEscape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}

// Search 实现 ticket.QueryStore。
func (s *Store) Search(ctx context.Context, q ticket.SearchQuery) ([]*ticket.Ticket, error) {
	where, args := searchWhere(q)
	order := "t.id DESC"
	if q.Order == ticket.OrderTriageAsc {
		// 按实际进入待确认的时间正序，同一时间按 ID 正序（稳定并列键）；游标为上一页最后一张的 (triage_since, id)
		order = "t.triage_since, t.id"
		if q.AfterID > 0 {
			args = append(args, q.AfterTriage, q.AfterID)
			where += fmt.Sprintf(" AND (t.triage_since, t.id) > ($%d::timestamptz, $%d::bigint)", len(args)-1, len(args))
		}
	} else if q.BeforeID > 0 {
		args = append(args, q.BeforeID)
		where += fmt.Sprintf(" AND t.id < $%d", len(args))
	}
	args = append(args, q.Limit)
	return s.tickets(ctx, "SELECT "+ticketColumns+" WHERE "+where+fmt.Sprintf(" ORDER BY %s LIMIT $%d", order, len(args)), args...)
}

// Stats 实现 ticket.QueryStore。
func (s *Store) Stats(ctx context.Context, q ticket.SearchQuery) ([]ticket.StatRow, error) {
	where, args := searchWhere(q)
	rows, err := s.db.QueryContext(ctx, `SELECT p.key, t.type, t.status, count(*)
		FROM lingo_tickets t JOIN lingo_projects p ON p.id = t.project_id LEFT JOIN lingo_repos r ON r.id = t.repo_id
		WHERE `+where+` GROUP BY p.key, t.type, t.status ORDER BY p.key, t.type, t.status`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: ticket stats: %w", err)
	}
	defer rows.Close()
	var out []ticket.StatRow
	for rows.Next() {
		var r ticket.StatRow
		if err := rows.Scan(&r.ProjectKey, &r.Type, &r.Status, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Events 实现 ticket.QueryStore。事件 ID 在事务内分配、提交顺序可能不同，直接按 ID 翻页时游标可能越过一个
// 尚未提交的较小 ID 而永远漏掉它，所以只返回早于所有进行中事务写入的事件（txid < 快照 xmin）。xmin 是整个实例
// 最老的进行中事务，宿主的长事务会把它拖住；因此写入超过 eventSettle 的事件无论如何都返回（灵工自己的事务
// 都是毫秒级，不会在这么久后才提交），变更流的延迟最多为 eventSettle。
func (s *Store) Events(ctx context.Context, q ticket.EventQuery) ([]notify.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.kind, e.created_at, COALESCE(e.project_id, 0), COALESCE(p.key, ''), COALESCE(e.ticket_id, 0),
		       e.status, e.prev_status, e.action, e.user_id, e.actor_id, e.payload
		FROM lingo_events e LEFT JOIN lingo_projects p ON p.id = e.project_id
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
		e.TicketType, e.Title, e.Body, e.URL, e.Perspectives = p.TicketType, p.Title, p.Body, p.URL, p.Perspectives
		out = append(out, e)
	}
	return out, rows.Err()
}

// Comments 实现 ticket.QueryStore：未删除的评论，按时间顺序。
func (s *Store) Comments(ctx context.Context, ticketID int64, publicOnly bool) ([]ticket.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ticket_id, COALESCE(gitea_comment_id, 0), source, author_id, author_name, body, public, created_at
		FROM lingo_ticket_comments WHERE ticket_id = $1 AND deleted_at IS NULL AND (NOT $2 OR public)
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
	rows, err := s.db.QueryContext(ctx, `SELECT p.key, e.key, e.value::boolean FROM lingo_projects p, jsonb_each_text(p.auto_forward) e
		WHERE p.key = ANY(($1::text)::text[])`, textArray(keys))
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
	if _, err := x.tx.ExecContext(ctx, "UPDATE lingo_tickets SET triage_reminded_at = $2 WHERE id = $1", id, at); err != nil {
		return fmt.Errorf("store: mark reminded %d: %w", id, err)
	}
	return nil
}
