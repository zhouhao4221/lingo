package assistant

import (
	"testing"
	"time"
)

func TestTimeRange(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, loc) // 星期二
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, loc) }
	for _, c := range []struct {
		spec     TimeSpec
		from, to time.Time
		ok       bool
	}{
		{TimeSpec{Preset: "today"}, d(2026, 11, 24), d(2026, 11, 25), true},
		{TimeSpec{Preset: "this_week"}, d(2026, 11, 23), d(2026, 11, 30), true},
		{TimeSpec{Preset: "last_week"}, d(2026, 11, 16), d(2026, 11, 23), true},
		{TimeSpec{Preset: "last_month"}, d(2026, 10, 1), d(2026, 11, 1), true},
		{TimeSpec{Preset: "last_7_days"}, d(2026, 11, 18), d(2026, 11, 25), true},
		{TimeSpec{From: "2026-11-01", To: "2026-11-02"}, d(2026, 11, 1), d(2026, 11, 3), true},
		{TimeSpec{From: "2026-11-05", To: "2026-11-01"}, time.Time{}, time.Time{}, false},
		{TimeSpec{From: "nope"}, time.Time{}, time.Time{}, false},
		{TimeSpec{}, time.Time{}, time.Time{}, true},
	} {
		from, to, ok := timeRange(c.spec, now, loc)
		if !from.Equal(c.from) || !to.Equal(c.to) || ok != c.ok {
			t.Errorf("%+v: %v %v %v", c.spec, from, to, ok)
		}
	}
	// 周日属于本周（周一开始）
	sun := time.Date(2026, 11, 29, 23, 0, 0, 0, loc)
	if from, _, _ := timeRange(TimeSpec{Preset: "this_week"}, sun, loc); !from.Equal(d(2026, 11, 23)) {
		t.Errorf("sunday week start = %v", from)
	}
	if got := today(now, loc); got != "2026-11-24 星期二" {
		t.Errorf("today = %q", got)
	}
}

func TestGroundedAndMentions(t *testing.T) {
	f := facts{Tickets: []factTicket{{ID: 12}}, Releases: []factRelease{{Tickets: []factRelTicket{{ID: 15}}}}}
	if !grounded("#12 已修复，#15 随版本发布", f) || grounded("#12 和 #99", f) || !grounded("没有编号", f) {
		t.Error("grounded")
	}
	for q, want := range map[string]bool{"在 #12 留言": true, "12号工单": true, "#123 留言": false, "v1.12.0": false, "112": false} {
		if got := mentions(q, 12); got != want {
			t.Errorf("mentions(%q) = %v", q, got)
		}
	}
	h := []HistoryItem{{Role: RoleUser}, {Role: RoleAssistant, Tickets: []TicketRef{{ID: 7}}}, {Role: RoleUser}}
	if id, ok := lastUniqueTicket(h); !ok || id != 7 {
		t.Errorf("last unique = %d %v", id, ok)
	}
	h = append(h, HistoryItem{Role: RoleAssistant, Tickets: []TicketRef{{ID: 1}, {ID: 2}}})
	if _, ok := lastUniqueTicket(h); ok {
		t.Error("ambiguous history ticket")
	}
}
