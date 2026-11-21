package tests

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/zhouhao4221/lingo"
)

func tableCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname = current_schema()").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 空库（GPCM 主库尚未执行 lingo 迁移）：未就绪但不是错误，检查不建表。
func TestCheckMigrationsEmptySchemaIsReadOnly(t *testing.T) {
	db := testDB(t, false)
	ctx := context.Background()
	st, err := lingo.CheckMigrations(ctx, db)
	if err != nil {
		t.Fatalf("CheckMigrations: %v", err)
	}
	if st.Ready || len(st.Pending) == 0 || st.Pending[0] != "0001_init.sql" || !slices.Equal(st.MissingTables, []string{"lingo_schema_migrations"}) {
		t.Errorf("status = %+v", st)
	}
	if n := tableCount(t, db); n != 0 {
		t.Errorf("check created %d tables", n)
	}
	// 早期没有 checksum 列的迁移记录表：只读检查不补列
	if _, err := db.Exec("CREATE TABLE lingo_schema_migrations (name text PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if st, err = lingo.CheckMigrations(ctx, db); err != nil || st.Ready || len(st.Pending) == 0 {
		t.Errorf("legacy table: %+v %v", st, err)
	}
	var cols int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema()
		AND table_name = 'lingo_schema_migrations'`).Scan(&cols); err != nil || cols != 1 {
		t.Errorf("columns = %d %v", cols, err)
	}
}

func TestCheckMigrationsStates(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	st, err := lingo.CheckMigrations(ctx, db)
	if err != nil || !st.Ready || len(st.Applied) == 0 || len(st.Pending)+len(st.Changed)+len(st.MissingTables)+len(st.Unknown) != 0 {
		t.Fatalf("after Migrate: %+v %v", st, err)
	}
	tables := tableCount(t, db)
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM lingo_schema_migrations").Scan(&rows); err != nil {
		t.Fatal(err)
	}

	// 库已由更新的版本迁移过：向前兼容，仍就绪
	if _, err := db.Exec("INSERT INTO lingo_schema_migrations (name, checksum) VALUES ('9999_future.sql', 'x')"); err != nil {
		t.Fatal(err)
	}
	if st, err = lingo.CheckMigrations(ctx, db); err != nil || !st.Ready || !slices.Equal(st.Unknown, []string{"9999_future.sql"}) {
		t.Errorf("newer db: %+v %v", st, err)
	}
	// 迁移内容被改：未就绪
	if _, err := db.Exec("UPDATE lingo_schema_migrations SET checksum = 'changed' WHERE name = '0001_init.sql'"); err != nil {
		t.Fatal(err)
	}
	if st, err = lingo.CheckMigrations(ctx, db); err != nil || st.Ready || !slices.Equal(st.Changed, []string{"0001_init.sql"}) {
		t.Errorf("changed: %+v %v", st, err)
	}
	// 缺表（如被误删）：未就绪并列出缺的表
	if _, err := db.Exec("DROP TABLE lingo_llm_usage"); err != nil {
		t.Fatal(err)
	}
	if st, err = lingo.CheckMigrations(ctx, db); err != nil || st.Ready || !slices.Contains(st.MissingTables, "lingo_llm_usage") {
		t.Errorf("missing table: %+v %v", st, err)
	}
	// 迁移记录缺失：待执行
	if _, err := db.Exec("DELETE FROM lingo_schema_migrations WHERE name = '0001_init.sql'"); err != nil {
		t.Fatal(err)
	}
	if st, err = lingo.CheckMigrations(ctx, db); err != nil || st.Ready || !slices.Contains(st.Pending, "0001_init.sql") {
		t.Errorf("pending: %+v %v", st, err)
	}
	// 以上检查均未修改数据：表数与迁移记录只随测试自己的语句变化
	var after int
	if err := db.QueryRow("SELECT count(*) FROM lingo_schema_migrations").Scan(&after); err != nil || after != rows {
		t.Errorf("migration rows %d -> %d", rows, after)
	}
	if n := tableCount(t, db); n != tables-1 {
		t.Errorf("tables %d -> %d", tables, n)
	}
}

// 查询失败（连接不可用、ctx 取消）返回错误，与“未就绪”区分。
func TestCheckMigrationsQueryError(t *testing.T) {
	db := testDB(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if st, err := lingo.CheckMigrations(ctx, db); err == nil || st.Ready {
		t.Errorf("canceled: %+v %v", st, err)
	}
	closed := testDB(t, false)
	closed.Close()
	if st, err := lingo.CheckMigrations(context.Background(), closed); err == nil || st.Ready {
		t.Errorf("closed db: %+v %v", st, err)
	}
}
