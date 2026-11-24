package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/zhouhao4221/lingo/internal/settings"
)

var _ settings.Store = (*Store)(nil)

const settingsColumns = `category, content, version, updated_by, updated_at`

func scanSettings(rows *sql.Rows) ([]settings.Record, error) {
	defer rows.Close()
	var out []settings.Record
	for rows.Next() {
		var r settings.Record
		var cat string
		var content []byte
		var at sql.NullTime
		if err := rows.Scan(&cat, &content, &r.Version, &r.UpdatedBy, &at); err != nil {
			return nil, err
		}
		r.Category, r.Content, r.UpdatedAt = settings.Category(cat), content, timePtr(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadSettings 实现 settings.Store。
func (s *Store) LoadSettings(ctx context.Context) ([]settings.Record, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+settingsColumns+" FROM lingo_settings ORDER BY category")
	if err != nil {
		return nil, fmt.Errorf("store: load settings: %w", err)
	}
	return scanSettings(rows)
}

// MutateSettings 实现 settings.Store：锁定全部行后读改写，并发修改不同字段不会互相覆盖。
func (s *Store) MutateSettings(ctx context.Context, by string, fn func([]settings.Record) (map[settings.Category]json.RawMessage, error)) ([]settings.Record, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交后回滚为空操作
	rows, err := tx.QueryContext(ctx, "SELECT "+settingsColumns+" FROM lingo_settings ORDER BY category FOR UPDATE")
	if err != nil {
		return nil, fmt.Errorf("store: lock settings: %w", err)
	}
	cur, err := scanSettings(rows)
	if err != nil {
		return nil, err
	}
	changes, err := fn(cur)
	if err != nil {
		return nil, err
	}
	for cat, content := range changes {
		if _, err := tx.ExecContext(ctx, `UPDATE lingo_settings SET content = $2, version = version + 1, updated_by = $3, updated_at = now()
			WHERE category = $1`, string(cat), []byte(content), by); err != nil {
			return nil, fmt.Errorf("store: save settings %s: %w", cat, err)
		}
	}
	rows, err = tx.QueryContext(ctx, "SELECT "+settingsColumns+" FROM lingo_settings ORDER BY category")
	if err != nil {
		return nil, fmt.Errorf("store: reload settings: %w", err)
	}
	out, err := scanSettings(rows)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit: %w", err)
	}
	return out, nil
}
