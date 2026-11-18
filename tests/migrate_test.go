package tests

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/zhouhao4221/lingo"
)

func TestMigrateFreshAndIdempotent(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	if err := lingo.Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name <> 'lingo_schema_migrations'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 20 {
		t.Errorf("tables = %d", n)
	}
	var name, sum string
	if err := db.QueryRowContext(ctx, "SELECT name, checksum FROM lingo_schema_migrations ORDER BY name LIMIT 1").Scan(&name, &sum); err != nil {
		t.Fatal(err)
	}
	if name != "0001_init.sql" || len(sum) != 64 {
		t.Errorf("migration record = %s %s", name, sum)
	}
}

func TestMigrateConcurrent(t *testing.T) {
	db := testDB(t, false)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- lingo.Migrate(context.Background(), db)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}
}

func TestMigrateDetectsModifiedMigration(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "UPDATE lingo_schema_migrations SET checksum = 'x' WHERE name = '0001_init.sql'"); err != nil {
		t.Fatal(err)
	}
	if err := lingo.Migrate(ctx, db); !errors.Is(err, lingo.ErrMigrationChanged) {
		t.Fatalf("err = %v, want ErrMigrationChanged", err)
	}
}

func TestMigrateUpgradesLegacyMigrationsTable(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	// 模拟早期版本建的迁移记录表：没有 checksum 列
	if _, err := db.ExecContext(ctx, "ALTER TABLE lingo_schema_migrations DROP COLUMN checksum"); err != nil {
		t.Fatal(err)
	}
	if err := lingo.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate on legacy table: %v", err)
	}
	var sum string
	if err := db.QueryRowContext(ctx, "SELECT checksum FROM lingo_schema_migrations WHERE name = '0001_init.sql'").Scan(&sum); err != nil || len(sum) != 64 {
		t.Errorf("checksum not backfilled: %q %v", sum, err)
	}
}

func TestMigrateChecksBeforeApplying(t *testing.T) {
	db := testDB(t, false)
	ctx := context.Background()
	// 记录表里有一个内容不符的“已执行”迁移，且尚未执行任何真实迁移：应先报错，不建任何表
	if _, err := db.ExecContext(ctx, `CREATE TABLE lingo_schema_migrations (name text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO lingo_schema_migrations (name, checksum) VALUES ('0001_init.sql', 'tampered')`); err != nil {
		t.Fatal(err)
	}
	if err := lingo.Migrate(ctx, db); !errors.Is(err, lingo.ErrMigrationChanged) {
		t.Fatalf("err = %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()").Scan(&n); err != nil || n != 1 {
		t.Errorf("tables after refused migrate = %d, %v", n, err)
	}
}
