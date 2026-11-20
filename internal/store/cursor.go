package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Cursor 返回仓库（"owner/name"）某类对账的游标；从未对账过时 ok 为 false。
func (s *Store) Cursor(ctx context.Context, repo, kind string) (time.Time, bool, error) {
	var at time.Time
	err := s.db.QueryRowContext(ctx, "SELECT at FROM lingo_repo_state WHERE repo = $1 AND kind = $2", strings.ToLower(repo), kind).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: cursor %s %s: %w", repo, kind, err)
	}
	return at, true, nil
}

// SetCursor 保存对账游标；只向后移动。
func (s *Store) SetCursor(ctx context.Context, repo, kind string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO lingo_repo_state (repo, kind, at) VALUES ($1, $2, $3)
		ON CONFLICT (repo, kind) DO UPDATE SET at = GREATEST(lingo_repo_state.at, EXCLUDED.at), updated_at = now()`,
		strings.ToLower(repo), kind, at)
	if err != nil {
		return fmt.Errorf("store: set cursor %s %s: %w", repo, kind, err)
	}
	return nil
}
