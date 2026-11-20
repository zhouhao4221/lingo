package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/zhouhao4221/lingo/internal/ticket"
)

// perspectiveOrUser 把空视角写为 user（列有 CHECK 约束）。
func perspectiveOrUser(p string) string {
	if p == "" {
		return ticket.PerspectiveUser
	}
	return p
}

func (x *ticketTx) AddPerspective(ctx context.Context, p ticket.Perspective) (int64, error) {
	at := p.At
	if at.IsZero() {
		at = time.Now()
	}
	var id int64
	if err := x.tx.QueryRowContext(ctx, `
		INSERT INTO lingo_ticket_perspectives (ticket_id, perspective, author_id, author_name, body, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING id`, p.TicketID, perspectiveOrUser(p.Perspective), p.AuthorID, p.AuthorName,
		p.Body, at).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: add perspective %d: %w", p.TicketID, err)
	}
	return id, nil
}

func (x *ticketTx) Perspectives(ctx context.Context, ticketID int64) ([]ticket.Perspective, error) {
	return queryPerspectives(ctx, x.tx, ticketID)
}

// Perspectives 实现 ticket.QueryStore：未撤回的视角补充，按时间顺序。
func (s *Store) Perspectives(ctx context.Context, ticketID int64) ([]ticket.Perspective, error) {
	return queryPerspectives(ctx, s.db, ticketID)
}

// PerspectivePosted 记录视角补充的提示评论已由机器人发到 Gitea。
func (s *Store) PerspectivePosted(ctx context.Context, id, giteaID int64) error {
	if _, err := s.db.ExecContext(ctx, "UPDATE lingo_ticket_perspectives SET gitea_posted_id = $2 WHERE id = $1", id, giteaID); err != nil {
		return fmt.Errorf("store: perspective posted %d: %w", id, err)
	}
	return nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryPerspectives(ctx context.Context, q queryer, ticketID int64) ([]ticket.Perspective, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, ticket_id, perspective, author_id, author_name, body, created_at, COALESCE(gitea_posted_id, 0)
		FROM lingo_ticket_perspectives WHERE ticket_id = $1 AND deleted_at IS NULL ORDER BY created_at, id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("store: perspectives %d: %w", ticketID, err)
	}
	defer rows.Close()
	var out []ticket.Perspective
	for rows.Next() {
		var p ticket.Perspective
		if err := rows.Scan(&p.ID, &p.TicketID, &p.Perspective, &p.AuthorID, &p.AuthorName, &p.Body, &p.At, &p.PostedID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
