package lingo

import (
	"fmt"
	"regexp"
	"slices"
	"testing"
)

func TestChecksumIgnoresCRLF(t *testing.T) {
	if checksum([]byte("CREATE TABLE a ();\r\nCREATE TABLE b ();\r\n")) != checksum([]byte("CREATE TABLE a ();\nCREATE TABLE b ();\n")) {
		t.Error("CRLF and LF checksums differ")
	}
	if checksum([]byte("a")) == checksum([]byte("b")) {
		t.Error("different content, same checksum")
	}
}

func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].name != "0001_init.sql" || len(ms[0].checksum) != 64 {
		t.Fatalf("migrations = %+v", ms)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i-1].name >= ms[i].name {
			t.Errorf("migrations out of order: %s, %s", ms[i-1].name, ms[i].name)
		}
	}
}

func TestMigrationStatus(t *testing.T) {
	ms := []migration{
		{name: "0001_init.sql", checksum: "c1", body: "CREATE TABLE lingo_a (id int);\ncreate table if not exists lingo_b (id int);"},
		{name: "0002_more.sql", checksum: "c2", body: "CREATE TABLE lingo_c (id int); DROP TABLE IF EXISTS lingo_b;"},
	}
	all := map[string]bool{"lingo_schema_migrations": true, "lingo_a": true, "lingo_c": true}
	for _, c := range []struct {
		name    string
		applied map[string]string
		tables  map[string]bool
		ready   bool
		want    MigrationStatus
	}{
		{"no migrations table", nil, map[string]bool{}, false,
			MigrationStatus{Applied: []string{}, Pending: []string{"0001_init.sql", "0002_more.sql"}, MissingTables: []string{"lingo_schema_migrations"}}},
		{"ready", map[string]string{"0001_init.sql": "c1", "0002_more.sql": "c2"}, all, true,
			MigrationStatus{Ready: true, Applied: []string{"0001_init.sql", "0002_more.sql"}}},
		{"legacy empty checksum", map[string]string{"0001_init.sql": "", "0002_more.sql": "c2"}, all, true,
			MigrationStatus{Ready: true, Applied: []string{"0001_init.sql", "0002_more.sql"}}},
		{"pending", map[string]string{"0001_init.sql": "c1"}, map[string]bool{"lingo_a": true, "lingo_b": true}, false,
			MigrationStatus{Applied: []string{"0001_init.sql"}, Pending: []string{"0002_more.sql"}}},
		{"changed", map[string]string{"0001_init.sql": "x", "0002_more.sql": "c2"}, all, false,
			MigrationStatus{Applied: []string{"0002_more.sql"}, Changed: []string{"0001_init.sql"}}},
		{"missing table", map[string]string{"0001_init.sql": "c1", "0002_more.sql": "c2"}, map[string]bool{"lingo_a": true}, false,
			MigrationStatus{Applied: []string{"0001_init.sql", "0002_more.sql"}, MissingTables: []string{"lingo_c"}}},
		{"newer database", map[string]string{"0001_init.sql": "c1", "0002_more.sql": "c2", "0003_future.sql": "f"}, all, true,
			MigrationStatus{Ready: true, Applied: []string{"0001_init.sql", "0002_more.sql"}, Unknown: []string{"0003_future.sql"}}},
	} {
		got := migrationStatus(ms, c.applied, c.tables)
		if fmt.Sprint(got) != fmt.Sprint(c.want) || got.Ready != c.ready {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// 内嵌迁移里解析出的表须覆盖 0001 建的全部表，检查才能发现缺表。
func TestMigrationTablesParsed(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	st := migrationStatus(ms, map[string]string{"0001_init.sql": ms[0].checksum}, map[string]bool{})
	want := []string{"lingo_projects", "lingo_repos", "lingo_releases", "lingo_tickets", "lingo_ticket_comments",
		"lingo_ticket_perspectives", "lingo_release_tickets", "lingo_ticket_events", "lingo_events", "lingo_outbox",
		"lingo_webhook_deliveries", "lingo_llm_usage", "lingo_repo_state"}
	if !slices.Equal(st.MissingTables, want) {
		t.Errorf("0001 tables = %v, want %v", st.MissingTables, want)
	}
	// 缺表推算的约束（PRD 5.4）：表名不带引号与 schema 前缀，不用 ALTER TABLE … RENAME 改表名
	bad := regexp.MustCompile(`(?i)\b(?:CREATE|DROP)\s+TABLE\s+(?:IF\s+(?:NOT\s+)?EXISTS\s+)?(?:"|[a-z_][a-z0-9_]*\s*\.)|\bALTER\s+TABLE\b[^;]*\bRENAME\s+TO\b`)
	for _, m := range ms {
		if b := bad.FindString(sqlCommentRe.ReplaceAllString(m.body, "")); b != "" {
			t.Errorf("%s: unsupported table statement for CheckMigrations: %q", m.name, b)
		}
	}
	// 注释里的 CREATE / DROP TABLE 不计入
	fake := []migration{{name: "0001_x.sql", checksum: "c", body: "-- CREATE TABLE lingo_ghost (\n/* DROP TABLE lingo_a; */\nCREATE TABLE lingo_a (id int);\n"}}
	if st := migrationStatus(fake, map[string]string{"0001_x.sql": "c"}, map[string]bool{}); !slices.Equal(st.MissingTables, []string{"lingo_a"}) {
		t.Errorf("comments parsed: %v", st.MissingTables)
	}
}
