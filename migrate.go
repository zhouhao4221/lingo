package lingo

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateLockKey 是迁移用的 PostgreSQL advisory lock 键，保证多实例同时调用时只有一个执行。
const migrateLockKey = 0x6c696e676f // "lingo"

var migrationNameRe = regexp.MustCompile(`^\d{4}_[a-z0-9_]+\.sql$`)

// ErrMigrationChanged 表示已执行过的迁移文件内容被修改（违反“已发布的迁移不得修改”）。
var ErrMigrationChanged = errors.New("lingo: applied migration has been modified")

type migration struct {
	name, body, checksum string
}

func loadMigrations() ([]migration, error) {
	paths, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(paths) // 四位编号前缀保证字典序即执行顺序
	ms := make([]migration, 0, len(paths))
	for _, p := range paths {
		name := strings.TrimPrefix(p, "migrations/")
		if !migrationNameRe.MatchString(name) {
			return nil, fmt.Errorf("lingo: invalid migration file name %q (want NNNN_name.sql)", name)
		}
		b, err := migrationFS.ReadFile(p)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		ms = append(ms, migration{name: name, body: string(b), checksum: hex.EncodeToString(sum[:])})
	}
	return ms, nil
}

// Migrate 按编号顺序执行尚未执行的迁移，每个文件一个事务；由接入方在发布流程中显式调用。
// 多实例同时调用时经 advisory lock 串行执行；已执行过的迁移若内容被修改，返回 ErrMigrationChanged 且不执行后续迁移。
// 迁移在 db 连接的当前 schema（search_path 第一项）中建表。
func Migrate(ctx context.Context, db *sql.DB) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("lingo: acquire migration lock: %w", err)
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrateLockKey) //nolint:errcheck // 连接关闭时锁也会释放

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS lingo_schema_migrations (
		name       text PRIMARY KEY,
		checksum   text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("lingo: create migrations table: %w", err)
	}
	applied := map[string]string{}
	rows, err := conn.QueryContext(ctx, "SELECT name, checksum FROM lingo_schema_migrations")
	if err != nil {
		return err
	}
	for rows.Next() {
		var n, c string
		if err := rows.Scan(&n, &c); err != nil {
			rows.Close()
			return err
		}
		applied[n] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range ms {
		if sum, ok := applied[m.name]; ok {
			if sum != m.checksum {
				return fmt.Errorf("%w: %s", ErrMigrationChanged, m.name)
			}
			continue
		}
		if err := apply(ctx, conn, m); err != nil {
			return fmt.Errorf("lingo: migrate %s: %w", m.name, err)
		}
	}
	return nil
}

func apply(ctx context.Context, conn *sql.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // 提交后回滚为空操作
	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO lingo_schema_migrations (name, checksum) VALUES ($1, $2)", m.name, m.checksum); err != nil {
		return err
	}
	return tx.Commit()
}
