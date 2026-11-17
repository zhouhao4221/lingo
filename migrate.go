package lingo

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateLockKey 是迁移用的 PostgreSQL advisory lock 键，保证多实例同时调用时只有一个执行。
const migrateLockKey = 0x6c696e676f // "lingo"

// Migrate 按编号顺序执行尚未执行的迁移，每个文件一个事务；由接入方在发布流程中显式调用。
func Migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrateLockKey)

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS lingo_schema_migrations (
		name text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, path := range names {
		name := strings.TrimPrefix(path, "migrations/")
		var exists bool
		if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM lingo_schema_migrations WHERE name = $1)", name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		body, err := migrationFS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := apply(ctx, conn, name, string(body)); err != nil {
			return fmt.Errorf("lingo: migrate %s: %w", name, err)
		}
	}
	return nil
}

func apply(ctx context.Context, conn *sql.Conn, name, body string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO lingo_schema_migrations (name) VALUES ($1)", name); err != nil {
		return err
	}
	return tx.Commit()
}
