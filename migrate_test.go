package lingo

import (
	"fmt"
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
	for _, n := range []string{"lingo_projects", "lingo_tickets", "lingo_outbox", "lingo_events"} {
		if !slices.Contains(st.MissingTables, n) {
			t.Errorf("%s not expected: %v", n, st.MissingTables)
		}
	}
}
