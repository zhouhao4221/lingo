package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

var _ ticket.Store = (*Store)(nil)

// InTx 实现 ticket.Store。
func (s *Store) InTx(ctx context.Context, fn func(ticket.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交后回滚为空操作
	if err := fn(&ticketTx{tx: tx, notify: s.notify}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// HasIssue 实现 ticket.Store。
func (s *Store) HasIssue(ctx context.Context, repo string, number int64) (bool, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || number <= 0 {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tickets t JOIN repos r ON r.id = t.repo_id
		WHERE lower(r.owner) = lower($1) AND lower(r.name) = lower($2) AND t.gitea_issue_number = $3)`, owner, name, number).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: has issue %s#%d: %w", repo, number, err)
	}
	return exists, nil
}

type ticketTx struct {
	tx     *sql.Tx
	notify bool
}

const ticketColumns = `t.id, t.project_id, p.key, COALESCE(t.repo_id, 0), COALESCE(r.owner || '/' || r.name, ''), t.type, t.title,
	t.status, t.user_id, COALESCE(t.gitea_issue_number, 0), t.gitea_updated_at, COALESCE(t.fixed_pr, 0), t.fixed_commit,
	t.milestone, t.due_date
	FROM tickets t JOIN projects p ON p.id = t.project_id LEFT JOIN repos r ON r.id = t.repo_id`

func (x *ticketTx) lock(ctx context.Context, where string, args ...any) (*ticket.Ticket, error) {
	var t ticket.Ticket
	var updated, due sql.NullTime
	err := x.tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" WHERE "+where+" FOR UPDATE OF t", args...).Scan(
		&t.ID, &t.ProjectID, &t.ProjectKey, &t.RepoID, &t.Repo, &t.Type, &t.Title, &t.Status, &t.UserID, &t.IssueNumber,
		&updated, &t.FixedPR, &t.FixedCommit, &t.Milestone, &due)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ticket.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: lock ticket: %w", err)
	}
	if updated.Valid {
		t.GiteaUpdatedAt = updated.Time
	}
	if due.Valid {
		d := due.Time
		t.DueDate = &d
	}
	return &t, nil
}

func (x *ticketTx) LockByIssue(ctx context.Context, repo string, number int64) (*ticket.Ticket, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || number <= 0 {
		return nil, ticket.ErrNotFound
	}
	return x.lock(ctx, "lower(r.owner) = lower($1) AND lower(r.name) = lower($2) AND t.gitea_issue_number = $3", owner, name, number)
}

func (x *ticketTx) LockByID(ctx context.Context, id int64) (*ticket.Ticket, error) {
	return x.lock(ctx, "t.id = $1", id)
}

func (x *ticketTx) Update(ctx context.Context, t *ticket.Ticket) error {
	var updated, due sql.NullTime
	if !t.GiteaUpdatedAt.IsZero() {
		updated = sql.NullTime{Time: t.GiteaUpdatedAt, Valid: true}
	}
	if t.DueDate != nil {
		due = sql.NullTime{Time: *t.DueDate, Valid: true}
	}
	_, err := x.tx.ExecContext(ctx, `
		UPDATE tickets SET status = $2, gitea_issue_number = $3, gitea_updated_at = $4, fixed_pr = $5, fixed_commit = $6,
		    milestone = $7, due_date = $8, updated_at = now()
		WHERE id = $1`,
		t.ID, string(t.Status), nullInt64(t.IssueNumber), updated, nullInt64(t.FixedPR), t.FixedCommit, t.Milestone, due)
	if err != nil {
		return fmt.Errorf("store: update ticket %d: %w", t.ID, err)
	}
	return nil
}

func (x *ticketTx) AddHistory(ctx context.Context, h ticket.History) error {
	payload, err := jsonObject(h.Payload)
	if err != nil {
		return err
	}
	if _, err := x.tx.ExecContext(ctx, "INSERT INTO ticket_events (ticket_id, actor_id, source, kind, payload) VALUES ($1, $2, $3, $4, $5)",
		h.TicketID, h.ActorID, h.Source, h.Kind, payload); err != nil {
		return fmt.Errorf("store: add ticket event %d: %w", h.TicketID, err)
	}
	return nil
}

func (x *ticketTx) SaveGiteaComment(ctx context.Context, c ticket.Comment) (*ticket.Comment, error) {
	at, known := c.At, !c.At.IsZero()
	if !known {
		at = time.Now()
	}
	// 先读旧值再写；调用方已在事务内锁定工单，同一评论不会被并发写入
	var prev ticket.Comment
	var deleted sql.NullTime
	err := x.tx.QueryRowContext(ctx, "SELECT ticket_id, gitea_comment_id, author_name, body, public, updated_at, deleted_at FROM ticket_comments WHERE gitea_comment_id = $1",
		c.GiteaID).Scan(&prev.TicketID, &prev.GiteaID, &prev.AuthorName, &prev.Body, &prev.Public, &prev.At, &deleted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = x.tx.ExecContext(ctx, `
			INSERT INTO ticket_comments (ticket_id, author_name, source, body, public, gitea_comment_id, created_at, updated_at)
			VALUES ($1, $2, 'gitea', $3, $4, $5, $6, $6)`,
			c.TicketID, c.AuthorName, c.Body, c.Public, c.GiteaID, at)
		if err != nil {
			return nil, fmt.Errorf("store: insert comment %d: %w", c.GiteaID, err)
		}
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read comment %d: %w", c.GiteaID, err)
	}
	if deleted.Valid || (known && at.Before(prev.At)) {
		// 已删除，或旧于已保存版本的乱序编辑：不写入，并按“可见性未变”返回，调用方不会因此发通知
		p := prev
		p.Public = p.Public || c.Public
		return &p, nil
	}
	if _, err := x.tx.ExecContext(ctx, "UPDATE ticket_comments SET body = $2, public = $3, author_name = $4, updated_at = $5 WHERE gitea_comment_id = $1",
		c.GiteaID, c.Body, c.Public, c.AuthorName, at); err != nil {
		return nil, fmt.Errorf("store: update comment %d: %w", c.GiteaID, err)
	}
	return &prev, nil
}

func (x *ticketTx) DeleteGiteaComment(ctx context.Context, giteaID int64, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := x.tx.ExecContext(ctx, "UPDATE ticket_comments SET deleted_at = $2 WHERE gitea_comment_id = $1 AND deleted_at IS NULL",
		giteaID, at); err != nil {
		return fmt.Errorf("store: delete comment %d: %w", giteaID, err)
	}
	return nil
}

// eventPayload 是 events.payload 中不单独成列的字段。
type eventPayload struct {
	TicketType string `json:"ticket_type,omitempty"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	URL        string `json:"url,omitempty"`
}

func (x *ticketTx) Emit(ctx context.Context, e notify.Event) error {
	payload, err := json.Marshal(eventPayload{TicketType: e.TicketType, Title: e.Title, Body: e.Body, URL: e.URL})
	if err != nil {
		return err
	}
	var id int64
	if err := x.tx.QueryRowContext(ctx, `
		INSERT INTO events (kind, project_id, ticket_id, user_id, actor_id, status, prev_status, action, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		string(e.Kind), nullInt64(e.ProjectID), nullInt64(e.TicketID), e.UserID, e.ActorID, e.Status, e.PrevStatus, e.Action, payload,
	).Scan(&id); err != nil {
		return fmt.Errorf("store: insert event: %w", err)
	}
	if !x.notify {
		return nil
	}
	if _, err := x.tx.ExecContext(ctx, "INSERT INTO outbox (kind, key, event_id) VALUES ('notify', $1, $2)",
		fmt.Sprintf("notify:event:%d", id), id); err != nil {
		return fmt.Errorf("store: enqueue notify %d: %w", id, err)
	}
	return nil
}

func jsonObject(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}
