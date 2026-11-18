package lingo

import "testing"

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
