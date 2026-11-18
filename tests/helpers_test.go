package tests

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/zhouhao4221/lingo"
)

// testDB 创建一个临时 schema 并返回 search_path 指向它的连接池；migrate 为真时先执行 lingo.Migrate。
func testDB(t *testing.T, migrate bool) *sql.DB {
	t.Helper()
	base := os.Getenv("LINGO_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("LINGO_TEST_DATABASE_URL not set")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "lingo_test_" + hex.EncodeToString(b)
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if migrate {
		if err := lingo.Migrate(ctx, db); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	return db
}
