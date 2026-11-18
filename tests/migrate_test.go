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
