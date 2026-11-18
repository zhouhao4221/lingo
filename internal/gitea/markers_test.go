package gitea

import (
	"fmt"
	"testing"
)

func TestTicketMarkerRoundTrip(t *testing.T) {
	body := "### 现象\n\n导出失败\n\n" + TicketMarker(42)
	if id := ParseTicketMarker(body); id != 42 {
		t.Errorf("ParseTicketMarker = %d", id)
	}
	if id := ParseTicketMarker("引用旧单 <!-- lingo:ticket=7 -->\n" + TicketMarker(42)); id != 42 {
		t.Errorf("last marker should win, got %d", id)
	}
	if id := ParseTicketMarker("no marker"); id != 0 {
		t.Errorf("no marker = %d", id)
	}
	if !HasTicketMarker(body, 42) || HasTicketMarker("x "+TicketMarker(420), 42) {
		t.Error("HasTicketMarker must match exactly")
	}
	if q := TicketMarkerQuery(42); q != "lingo:ticket=42" {
		t.Errorf("query = %q", q)
	}
}

func TestPublicReply(t *testing.T) {
	cases := []struct {
		body   string
		public bool
		text   string
	}{
		{"/reply 已修复，请升级到 v1.3", true, "已修复，请升级到 v1.3"},
		{"  \n/reply: 请提供日志", true, "请提供日志"},
		{"/reply：请提供日志", true, "请提供日志"},
		{"/reply ：已修复", true, "已修复"},
		{"/reply", true, ""},
		{"已定位原因\n\n" + PublicMarker, true, "已定位原因"},
		{"/replying to the reviewer", false, "/replying to the reviewer"},
		{"内部讨论：先看日志 /reply 之后再说", false, "内部讨论：先看日志 /reply 之后再说"},
	}
	for _, c := range cases {
		if got := IsPublicReply(c.body); got != c.public {
			t.Errorf("IsPublicReply(%q) = %v", c.body, got)
		}
		if got := PublicReplyText(c.body); got != c.text {
			t.Errorf("PublicReplyText(%q) = %q, want %q", c.body, got, c.text)
		}
	}
}

func TestParseIssueRefs(t *testing.T) {
	text := "Fixes #12 and closes other/repo#3.\n" +
		"Related to #5 (see #12 again), Reopens #8\n" +
		"Resolved: o/r#9\n" +
		"`#100` in code, and\n```\nfixes #101\n```\n" +
		"not a ref: abc#7, issue#6"
	got := fmt.Sprint(ParseIssueRefs(text))
	want := fmt.Sprint([]IssueRef{
		{Index: 12, Action: RefCloses},
		{Owner: "other", Repo: "repo", Index: 3, Action: RefCloses},
		{Index: 5},
		{Index: 8, Action: RefReopens},
		{Owner: "o", Repo: "r", Index: 9, Action: RefCloses},
	})
	if got != want {
		t.Errorf("ParseIssueRefs\n got: %s\nwant: %s", got, want)
	}
}

func TestParseIssueRefsMentionUpgradedToClose(t *testing.T) {
	refs := ParseIssueRefs("See #4. Fix #4")
	if len(refs) != 1 || refs[0].Action != RefCloses {
		t.Errorf("refs = %+v", refs)
	}
}
