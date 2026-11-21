package lingo

import (
	"bytes"
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
		ms = append(ms, migration{name: name, body: string(b), checksum: checksum(b)})
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
	// 兼容早期没有 checksum 列的迁移记录表（仅开发环境出现过）：补列，空值在下面按当前内容回填
	if _, err := conn.ExecContext(ctx, "ALTER TABLE lingo_schema_migrations ADD COLUMN IF NOT EXISTS checksum text NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("lingo: upgrade migrations table: %w", err)
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
	// 先校验全部已执行迁移，再执行新的：避免执行了一部分才发现旧迁移被改
	for _, m := range ms {
		sum, ok := applied[m.name]
		switch {
		case !ok:
		case sum == "":
			if _, err := conn.ExecContext(ctx, "UPDATE lingo_schema_migrations SET checksum = $2 WHERE name = $1", m.name, m.checksum); err != nil {
				return fmt.Errorf("lingo: backfill checksum %s: %w", m.name, err)
			}
		case sum != m.checksum:
			return fmt.Errorf("%w: %s", ErrMigrationChanged, m.name)
		}
	}
	for _, m := range ms {
		if _, ok := applied[m.name]; ok {
			continue
		}
		if err := apply(ctx, conn, m); err != nil {
			return fmt.Errorf("lingo: migrate %s: %w", m.name, err)
		}
	}
	return nil
}

// checksum 按 LF 计算 sha256：Windows 下 core.autocrlf 签出的 CRLF 文件不应被当成已修改。
func checksum(b []byte) string {
	sum := sha256.Sum256(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// MigrationStatus 是 CheckMigrations 的结果：本版本内嵌的迁移与数据库当前 schema 的对比。
type MigrationStatus struct {
	// Ready 为真表示本版本需要的迁移都已执行、内容一致且应有的表都在，可以正常使用 lingo。
	Ready   bool     `json:"ready"`
	Applied []string `json:"applied"`           // 已执行且内容与本版本一致的迁移
	Pending []string `json:"pending,omitempty"` // 本版本包含、数据库尚未执行的迁移（需调用 Migrate）
	// Changed 是已执行、但内容与本版本不同的迁移（Migrate 会返回 ErrMigrationChanged，需人工处理）。
	Changed []string `json:"changed,omitempty"`
	// Unknown 是数据库已执行、本版本没有的迁移（已由更新的版本迁移过）；迁移向前兼容，不影响 Ready。
	Unknown []string `json:"unknown,omitempty"`
	// MissingTables 是已执行的迁移应当建出、当前 schema 中却不存在的表；迁移记录表本身不存在时为 lingo_schema_migrations。
	MissingTables []string `json:"missing_tables,omitempty"`
}

// 缺表推算只从迁移 SQL 中识别 CREATE TABLE / DROP TABLE（去掉 -- 与 /* */ 注释后匹配），表名须是不带引号、不带
// schema 前缀的小写标识符，且不用 ALTER TABLE … RENAME 改表名；TestMigrationTablesParsed 对内嵌迁移校验这些约束（PRD 5.4）。
var (
	sqlCommentRe  = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*`)
	createTableRe = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	dropTableRe   = regexp.MustCompile(`(?i)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
)

// CheckMigrations 只读地检查 db 当前 schema（search_path 第一项，与 Migrate 相同）的迁移是否就绪，供接入方在
// 迁移未执行、缺表时降级（如只禁用反馈入口）而不影响宿主其他功能。检查在只读事务中进行：不建表、不取 advisory lock、
// 不回填校验和，也不修改任何数据；可在 New 之前、任意实例上频繁调用。查询只读系统目录与迁移记录表，取的是普通读锁
// （AccessShareLock），遇到正在执行、持有排他锁的 DDL（如 Migrate 进行中）时会等待，调用方应给 ctx 设置超时。
//
// 返回 err == nil 时，MigrationStatus.Ready 说明是否就绪（未执行迁移、缺表、迁移内容被改都是 Ready=false，
// 不是错误）；返回 err != nil 表示检查本身失败（连接失败、超时、权限不足、ctx 取消等），此时无法判断是否就绪，
// 调用方应按“状态未知”处理（重试或告警），不要当作未就绪去执行迁移。
func CheckMigrations(ctx context.Context, db *sql.DB) (MigrationStatus, error) {
	ms, err := loadMigrations()
	if err != nil {
		return MigrationStatus{}, err
	}
	applied, tables, err := readMigrationState(ctx, db)
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("lingo: check migrations: %w", err)
	}
	return migrationStatus(ms, applied, tables), nil
}

// readMigrationState 在只读事务中读取当前 schema 的表名与迁移记录；迁移记录表不存在时 applied 为 nil。
func readMigrationState(ctx context.Context, db *sql.DB) (applied map[string]string, tables map[string]bool, err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 只读事务，回滚即结束
	rows, err := tx.QueryContext(ctx, "SELECT tablename FROM pg_catalog.pg_tables WHERE schemaname = current_schema()")
	if err != nil {
		return nil, nil, err
	}
	tables = map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		tables[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if !tables["lingo_schema_migrations"] {
		return nil, tables, nil
	}
	// 早期迁移记录表可能没有 checksum 列（Migrate 会补列）：只读检查不补列，按空校验和处理
	var hasChecksum bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'lingo_schema_migrations' AND column_name = 'checksum')`).Scan(&hasChecksum); err != nil {
		return nil, nil, err
	}
	q := "SELECT name, '' FROM lingo_schema_migrations"
	if hasChecksum {
		q = "SELECT name, checksum FROM lingo_schema_migrations"
	}
	if rows, err = tx.QueryContext(ctx, q); err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	applied = map[string]string{}
	for rows.Next() {
		var n, c string
		if err := rows.Scan(&n, &c); err != nil {
			return nil, nil, err
		}
		applied[n] = c
	}
	return applied, tables, rows.Err()
}

// migrationStatus 对比内嵌迁移与数据库状态。applied 为 nil 表示迁移记录表不存在；空校验和（早期记录，Migrate 会按
// 当前内容回填）视为一致。应有的表按已执行迁移中的 CREATE TABLE / DROP TABLE 依次推算。
func migrationStatus(ms []migration, applied map[string]string, tables map[string]bool) MigrationStatus {
	st := MigrationStatus{Applied: []string{}}
	if applied == nil {
		for _, m := range ms {
			st.Pending = append(st.Pending, m.name)
		}
		st.MissingTables = []string{"lingo_schema_migrations"}
		return st
	}
	known := map[string]bool{}
	want := map[string]bool{}
	var order []string
	for _, m := range ms {
		known[m.name] = true
		sum, ok := applied[m.name]
		switch {
		case !ok:
			st.Pending = append(st.Pending, m.name)
			continue
		case sum != "" && sum != m.checksum:
			st.Changed = append(st.Changed, m.name)
		default:
			st.Applied = append(st.Applied, m.name)
		}
		body := sqlCommentRe.ReplaceAllString(m.body, "")
		for _, mt := range createTableRe.FindAllStringSubmatch(body, -1) {
			if n := strings.ToLower(mt[1]); !want[n] {
				want[n] = true
				order = append(order, n)
			}
		}
		for _, mt := range dropTableRe.FindAllStringSubmatch(body, -1) {
			delete(want, strings.ToLower(mt[1]))
		}
	}
	for n := range applied {
		if !known[n] {
			st.Unknown = append(st.Unknown, n)
		}
	}
	sort.Strings(st.Unknown)
	for _, n := range order {
		if want[n] && !tables[n] {
			st.MissingTables = append(st.MissingTables, n)
		}
	}
	st.Ready = len(st.Pending) == 0 && len(st.Changed) == 0 && len(st.MissingTables) == 0
	return st
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
