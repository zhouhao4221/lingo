package assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestGroundedStreamHoldsInventedNumbers(t *testing.T) {
	var out strings.Builder
	g := &groundedStream{ids: []int64{12}, out: func(s string) { out.WriteString(s) }}
	for _, d := range []string{"#1", "2 已修复，", "另有 #", "9", "9 也修了"} {
		g.write(d)
	}
	if got := out.String(); got != "#12 已修复，另有 " || !g.bad {
		t.Errorf("forwarded %q, bad=%v", got, g.bad)
	}
	out.Reset()
	g = &groundedStream{ids: []int64{12}, out: func(s string) { out.WriteString(s) }}
	g.write("见 #")
	g.write("12")
	g.flush()
	if out.String() != "见 #12" {
		t.Errorf("flush = %q", out.String())
	}
	if got := plainRefs("修复登录 (#45)、demo/api#7"); got != "修复登录 (Issue 45)、demo/apiIssue 7" {
		t.Errorf("plainRefs = %q", got)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "Client.Timeout exceeded while reading body" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestErrKind(t *testing.T) {
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, c := range []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{ctx, fmt.Errorf("llm: read stream: %w", timeoutErr{}), ErrKindTimeout},
		{ctx, context.DeadlineExceeded, ErrKindTimeout},
		{canceled, errors.New("x"), ErrKindCanceled},
		{ctx, modelError{errors.New("model down")}, ErrKindModel},
		{ctx, errors.New("db down"), ErrKindInternal},
	} {
		if got := errKind(c.ctx, c.err); got != c.want {
			t.Errorf("errKind(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}

func TestGroundedAndMentions(t *testing.T) {
	f := facts{Tickets: []factTicket{{ID: 12}}, Releases: []factRelease{{Tickets: []factRelTicket{{ID: 15}}}}}
	ids := factIDs(f)
	if !grounded("#12 已修复，#15 随版本发布", ids) || grounded("#12 和 #99", ids) || !grounded("没有编号", ids) {
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
