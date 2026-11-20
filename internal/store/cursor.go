package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Cursor 返回仓库（"owner/name"）某类对账的游标；从未对账过时 ok 为 false。仓库不在库中时同样 ok 为 false。
func (s *Store) Cursor(ctx context.Context, repo, kind string) (time.Time, bool, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var at time.Time
	err := s.db.QueryRowContext(ctx, `SELECT c.since FROM sync_cursors c JOIN repos r ON r.id = c.repo_id
		WHERE lower(r.owner) = lower($1) AND lower(r.name) = lower($2) AND c.kind = $3`, owner, name, kind).Scan(&at)
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
	owner, name, _ := strings.Cut(repo, "/")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_cursors (repo_id, kind, since)
		SELECT id, $3, $4 FROM repos WHERE lower(owner) = lower($1) AND lower(name) = lower($2)
		ON CONFLICT (repo_id, kind) DO UPDATE SET since = GREATEST(sync_cursors.since, EXCLUDED.since), updated_at = now()`,
		owner, name, kind, at)
	if err != nil {
		return fmt.Errorf("store: set cursor %s %s: %w", repo, kind, err)
	}
	return nil
}
