package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/zhouhao4221/lingo/internal/connector"
)

var _ connector.AuditStore = (*Store)(nil)

// 错误不包装语句参数：调用方只记录错误码。

func argsJSON(m map[string]string) (sql.NullString, error) {
	if m == nil {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(m)
	return sql.NullString{String: string(b), Valid: true}, err
}

// InsertDataRequest 实现 connector.AuditStore。
func (s *Store) InsertDataRequest(ctx context.Context, r *connector.Record, lease time.Duration) error {
	args, err := argsJSON(r.Args)
	if err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx, `INSERT INTO lingo_data_requests (user_id, user_role, project_key, tool, channel, conversation_id,
			arg_names, args, status, reason, duration_ms, truncated, lease_until, finished_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, ($7::text)::text[], $8::jsonb, $9, $10, $11, $12,
			CASE WHEN $9 = 'pending' THEN now() + $13::bigint * interval '1 millisecond' END,
			CASE WHEN $9 = 'pending' THEN NULL ELSE now() END)
		RETURNING id, created_at`,
		r.UserID, r.UserRole, r.Project, r.Tool, r.Channel, nullInt64(r.ConversationID), textArray(r.ArgNames), args,
		r.Status, r.Reason, r.Duration.Milliseconds(), r.Truncated, lease.Milliseconds()).Scan(&r.ID, &r.CreatedAt)
}

// FinishDataRequest 实现 connector.AuditStore。
func (s *Store) FinishDataRequest(ctx context.Context, r *connector.Record) error {
	args, err := argsJSON(r.Args)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE lingo_data_requests SET project_key = NULLIF($2, ''), arg_names = ($3::text)::text[],
			args = $4::jsonb, status = $5, reason = $6, duration_ms = $7, truncated = $8, lease_until = NULL, finished_at = now()
		WHERE id = $1 AND status = 'pending'`,
		r.ID, r.Project, textArray(r.ArgNames), args, r.Status, r.Reason, r.Duration.Milliseconds(), r.Truncated)
	return err
}

// ListDataRequests 实现 connector.AuditStore。
func (s *Store) ListDataRequests(ctx context.Context, q connector.Query) ([]connector.Record, error) {
	var since, until sql.NullTime
	if !q.Since.IsZero() {
		since = sql.NullTime{Time: q.Since, Valid: true}
	}
	if !q.Until.IsZero() {
		until = sql.NullTime{Time: q.Until, Valid: true}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, user_role, COALESCE(project_key, ''), tool, channel,
			COALESCE(conversation_id, 0), array_to_json(arg_names)::text, args::text, status, reason, duration_ms, truncated, created_at, finished_at
		FROM lingo_data_requests
		WHERE (user_id = $1 OR project_key = ANY(($2::text)::text[]))
		  AND ($3 = '' OR tool = $3) AND ($4 = '' OR user_id = $4) AND ($5 = '' OR status = $5)
		  AND ($6::timestamptz IS NULL OR created_at >= $6) AND ($7::timestamptz IS NULL OR created_at < $7)
		  AND ($8::bigint = 0 OR id < $8)
		ORDER BY id DESC LIMIT $9`,
		q.ViewerID, textArray(q.Projects), q.Tool, q.UserID, q.Status, since, until, q.BeforeID, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []connector.Record
	for rows.Next() {
		var r connector.Record
		var names string
		var args sql.NullString
		var ms int64
		var fin sql.NullTime
		if err := rows.Scan(&r.ID, &r.UserID, &r.UserRole, &r.Project, &r.Tool, &r.Channel, &r.ConversationID, &names, &args,
			&r.Status, &r.Reason, &ms, &r.Truncated, &r.CreatedAt, &fin); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(names), &r.ArgNames); err != nil {
			return nil, err
		}
		if args.Valid {
			if err := json.Unmarshal([]byte(args.String), &r.Args); err != nil {
				return nil, err
			}
		}
		r.Duration = time.Duration(ms) * time.Millisecond
		if fin.Valid {
			t := fin.Time
			r.FinishedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InterruptDataRequests 实现 connector.AuditStore：只处理租约已过期的记录（租约按数据库时间计算）。
func (s *Store) InterruptDataRequests(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE lingo_data_requests SET status = 'interrupted', reason = 'interrupted',
			lease_until = NULL, finished_at = now()
		WHERE status = 'pending' AND lease_until < now()`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PurgeDataRequests 实现 connector.AuditStore：截止时间按数据库时钟计算。
func (s *Store) PurgeDataRequests(ctx context.Context, retention time.Duration) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM lingo_data_requests WHERE created_at < now() - $1::bigint * interval '1 millisecond'",
		retention.Milliseconds())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
