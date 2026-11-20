package ticket

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/notify"
)

var (
	alice    = auth.Principal{ID: "u1", Name: "Alice", Role: auth.RoleExternal}
	bob      = auth.Principal{ID: "u2", Role: auth.RoleExternal}
	reviewer = auth.Principal{ID: "r1", Name: "Rita", Role: auth.RoleReviewer, Projects: []string{"demo"}}
	outsider = auth.Principal{ID: "r2", Role: auth.RoleReviewer, Projects: []string{"other"}}
	agent    = auth.Principal{ID: "a1", Role: auth.RoleAgent}
	admin    = auth.Principal{ID: "root", Role: auth.RoleAdmin}
)

func newService(t *testing.T, m *memStore, auto map[string]bool) *Service {
	t.Helper()
	s, err := NewService(ServiceOptions{Store: m, AutoForward: func(p, typ string) bool { return auto[typ] }, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func kinds(es []notify.Event) []string {
	var out []string
	for _, e := range es {
		out = append(out, string(e.Kind))
	}
	return out
}

func TestCreateAutoForwardAndTriage(t *testing.T) {
	m := newMem()
	s := newService(t, m, map[string]bool{TypeBug: true})
	ctx := context.Background()
	bug, err := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "\n## 登录后白屏\n点击登录按钮后页面空白"})
	if err != nil {
		t.Fatal(err)
	}
	if bug.Status != StatusSubmitted || bug.Repo != "demo/api" || bug.Title != "登录后白屏" || bug.TriageSince != nil {
		t.Errorf("bug = %+v", bug)
	}
	if _, ok := m.tasks["create_issue:ticket:1"]; !ok || len(m.tasks) != 1 {
		t.Errorf("tasks = %+v", m.tasks)
	}
	feat, err := s.Create(ctx, alice, NewTicket{Project: "demo", Repo: "Demo/Web", Type: TypeFeature, Title: "导出 Excel", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if feat.Status != StatusPendingTriage || feat.Repo != "demo/web" || feat.TriageSince == nil || len(m.tasks) != 1 {
		t.Errorf("feature = %+v tasks %d", feat, len(m.tasks))
	}
	if got := kinds(m.events); len(got) != 3 || got[0] != "ticket.created" || got[1] != "ticket.created" || got[2] != "triage.required" {
		t.Errorf("events = %v", got)
	}
	// 运行时覆盖优先于代码默认值
	on := false
	if err := s.SetAutoForward(ctx, admin, "demo", TypeBug, &on); err != nil {
		t.Fatal(err)
	}
	if b2, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "y"}); b2.Status != StatusPendingTriage {
		t.Errorf("override ignored: %s", b2.Status)
	}
	if err := s.SetAutoForward(ctx, reviewer, "demo", TypeBug, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("reviewer set auto-forward: %v", err)
	}
	// 项目没有仓库：即使开启自动转出也进入待确认
	m.auto["2:consult"] = true
	if c, _ := s.Create(ctx, alice, NewTicket{Project: "empty", Type: TypeConsult, Body: "z"}); c.Status != StatusPendingTriage {
		t.Errorf("no repo: %s", c.Status)
	}
}

func TestCreateValidation(t *testing.T) {
	s := newService(t, newMem(), nil)
	ctx := context.Background()
	for name, c := range map[string]struct {
		p    auth.Principal
		in   NewTicket
		want error
	}{
		"bad type":       {alice, NewTicket{Project: "demo", Type: "question", Body: "x"}, ErrInvalid},
		"empty":          {alice, NewTicket{Project: "demo", Type: TypeBug}, ErrInvalid},
		"repo elsewhere": {alice, NewTicket{Project: "demo", Repo: "x/y", Type: TypeBug, Body: "x"}, ErrInvalid},
		"no project":     {alice, NewTicket{Project: "nope", Type: TypeBug, Body: "x"}, ErrNotFound},
		"out of scope":   {outsider, NewTicket{Project: "demo", Type: TypeBug, Body: "x"}, ErrNotFound},
		"anonymous":      {auth.Principal{Role: auth.RoleExternal}, NewTicket{Project: "demo", Type: TypeBug, Body: "x"}, ErrForbidden},
	} {
		if _, err := s.Create(ctx, c.p, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func TestTriageActions(t *testing.T) {
	m := newMem()
	s := newService(t, m, nil)
	ctx := context.Background()
	mk := func() *Ticket {
		tk, err := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeFeature, Body: "需求"})
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	a, b, c, d := mk(), mk(), mk(), mk()
	m.events = nil

	if _, err := s.Forward(ctx, alice, a.ID, ForwardInput{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("external forward: %v", err)
	}
	if _, err := s.Forward(ctx, outsider, a.ID, ForwardInput{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("out-of-project forward: %v", err)
	}
	fa, err := s.Forward(ctx, reviewer, a.ID, ForwardInput{Title: "支持导出", Type: TypeBug, Repo: "demo/web", Note: "优先"})
	if err != nil {
		t.Fatal(err)
	}
	if fa.Status != StatusSubmitted || fa.Title != "支持导出" || fa.Type != TypeBug || fa.Repo != "demo/web" || fa.TriageSince != nil {
		t.Errorf("forwarded = %+v", fa)
	}
	task := m.tasks["create_issue:ticket:1"]
	if task.Payload["forwarded_by"] != "Rita" || task.Payload["note"] != "优先" {
		t.Errorf("task = %+v", task)
	}
	if _, err := s.Forward(ctx, reviewer, a.ID, ForwardInput{}); !errors.Is(err, ErrConflict) {
		t.Errorf("forward twice: %v", err)
	}
	if _, err := s.Answer(ctx, reviewer, b.ID, "  "); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty answer: %v", err)
	}
	if ab, err := s.Answer(ctx, reviewer, b.ID, "在设置里可以导出"); err != nil || ab.Status != StatusAnswered || len(m.notes) != 1 || !m.notes[0].Public {
		t.Errorf("answer: %v %+v %+v", err, ab, m.notes)
	}
	if _, err := s.Reject(ctx, reviewer, c.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("reject without reason: %v", err)
	}
	if rc, err := s.Reject(ctx, reviewer, c.ID, "不在范围内"); err != nil || rc.Status != StatusRejected {
		t.Errorf("reject: %v %+v", err, rc)
	}
	if _, err := s.Merge(ctx, reviewer, d.ID, c.ID, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("merge into rejected: %v", err)
	}
	if md, err := s.Merge(ctx, reviewer, d.ID, a.ID, "重复"); err != nil || md.Status != StatusMerged || md.MergedInto != a.ID {
		t.Errorf("merge: %v %+v", err, md)
	}
	// 每个处理动作：ticket.status + triage.resolved（带 Action 与 ActorID）
	var resolved []string
	for _, e := range m.events {
		if e.Kind == notify.TriageResolved {
			if e.ActorID != "r1" || e.PrevStatus != string(StatusPendingTriage) {
				t.Errorf("resolved event = %+v", e)
			}
			resolved = append(resolved, e.Action)
		}
	}
	if len(resolved) != 4 || resolved[0] != "forward" || resolved[3] != "merge" || len(m.triages) != 4 {
		t.Errorf("resolved = %v triages %d", resolved, len(m.triages))
	}
	if len(m.tasks) != 1 {
		t.Errorf("tasks = %d", len(m.tasks))
	}
}

func TestVisibility(t *testing.T) {
	m := newMem()
	s := newService(t, m, map[string]bool{TypeBug: true})
	ctx := context.Background()
	fwd, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "x"})
	pending, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeFeature, Body: "y"})
	for _, c := range []struct {
		p    auth.Principal
		id   int64
		want bool
	}{
		{alice, fwd.ID, true}, {bob, fwd.ID, false},
		{agent, fwd.ID, true}, {agent, pending.ID, false},
		{reviewer, pending.ID, true}, {outsider, pending.ID, false},
		{auth.Principal{ID: "u1", Role: "guest"}, fwd.ID, false},
	} {
		_, err := s.Get(ctx, c.p, c.id)
		if (err == nil) != c.want || (err != nil && !errors.Is(err, ErrNotFound)) {
			t.Errorf("%s/%s get %d: %v", c.p.Role, c.p.ID, c.id, err)
		}
	}
}

func TestMergeLocksBothAndRejectsCycles(t *testing.T) {
	m := newMem()
	s := newService(t, m, nil)
	ctx := context.Background()
	a, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "a"})
	b, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "b"})
	if _, err := s.Merge(ctx, reviewer, b.ID, a.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Merge(ctx, reviewer, a.ID, b.ID, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("reverse merge: %v", err)
	}
	if _, err := s.Merge(ctx, reviewer, a.ID, 999, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("missing target: %v", err)
	}
}

func TestForwardRevalidatesRepo(t *testing.T) {
	m := newMem()
	s := newService(t, m, nil)
	ctx := context.Background()
	tk, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Repo: "demo/web", Type: TypeBug, Body: "x"})
	m.repos[1] = m.repos[1][:1] // demo/web 已从项目移除
	f, err := s.Forward(ctx, reviewer, tk.ID, ForwardInput{})
	if err != nil || f.Repo != "demo/api" {
		t.Errorf("forward = %+v %v", f, err)
	}
}

func TestSummarizeStable(t *testing.T) {
	f := map[string][]string{"version": {"1.2"}, "steps": {"click save"}, "a": {""}}
	for range 20 {
		if got := summarize("", f); got != "click save" {
			t.Fatalf("summarize = %q", got)
		}
	}
}

func TestIssueFailedMarksAbnormal(t *testing.T) {
	m := newMem()
	s := newService(t, m, map[string]bool{TypeBug: true})
	ctx := context.Background()
	tk, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "x"})
	if err := s.IssueFailed(ctx, tk.ID, "403"); err != nil {
		t.Fatal(err)
	}
	if got := m.tickets[tk.ID]; got.Status != StatusAbnormal || m.events[len(m.events)-1].PrevStatus != string(StatusSubmitted) {
		t.Errorf("ticket = %+v", got)
	}
}

func TestReforwardAfterIssueFailed(t *testing.T) {
	m := newMem()
	s := newService(t, m, map[string]bool{TypeBug: true})
	ctx := context.Background()
	tk, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "x"})
	_ = s.IssueFailed(ctx, tk.ID, "403")
	f, err := s.Forward(ctx, reviewer, tk.ID, ForwardInput{})
	if err != nil || f.Status != StatusSubmitted || len(m.tasks) != 2 {
		t.Errorf("re-forward: %+v %v tasks %d", f, err, len(m.tasks))
	}
	if _, err := s.Answer(ctx, reviewer, tk.ID, "x"); !errors.Is(err, ErrConflict) {
		t.Errorf("answer submitted ticket: %v", err)
	}
}

func TestCreateConversationAndEmptyFields(t *testing.T) {
	s := newService(t, newMem(), nil)
	ctx := context.Background()
	if _, err := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "x", Conversation: 1}); err != nil {
		t.Errorf("own conversation: %v", err)
	}
	if _, err := s.Create(ctx, bob, NewTicket{Project: "demo", Type: TypeBug, Body: "x", Conversation: 1}); !errors.Is(err, ErrInvalid) {
		t.Errorf("other's conversation: %v", err)
	}
	if _, err := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Fields: map[string][]string{"a": {" "}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("blank fields: %v", err)
	}
	long := strings.Repeat("长", 300)
	if tk, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Title: long, Body: "x"}); len([]rune(tk.Title)) != MaxTitleRunes {
		t.Errorf("title runes = %d", len([]rune(tk.Title)))
	}
}

func TestRemindOverdue(t *testing.T) {
	m := newMem()
	now := t0
	s, _ := NewService(ServiceOptions{Store: m, Now: func() time.Time { return now }})
	ctx := context.Background()
	tk, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeFeature, Body: "x"})
	count := func() int {
		n := 0
		for _, e := range m.events {
			if e.Kind == notify.TriageOverdue && e.TicketID == tk.ID {
				n++
			}
		}
		return n
	}
	for _, step := range []struct {
		after time.Duration
		want  int
	}{{time.Hour, 0}, {25 * time.Hour, 1}, {26 * time.Hour, 1}, {50 * time.Hour, 2}} {
		now = t0.Add(step.after)
		if _, err := s.RemindOverdue(ctx, 24*time.Hour, 10); err != nil {
			t.Fatal(err)
		}
		if got := count(); got != step.want {
			t.Errorf("after %v: reminders = %d, want %d", step.after, got, step.want)
		}
	}
}

func TestListEventsScopesByRole(t *testing.T) {
	m := newMem()
	s := newService(t, m, nil)
	ctx := context.Background()
	for _, c := range []struct {
		p    auth.Principal
		want EventQuery
	}{
		{alice, EventQuery{Scope: Scope{All: true}, UserID: "u1", ExcludeTriage: true}},
		{agent, EventQuery{Scope: Scope{All: true}, ExcludeTriage: true, ForwardedOnly: true}},
		{reviewer, EventQuery{Scope: Scope{Projects: []string{"demo"}}}},
	} {
		if _, err := s.ListEvents(ctx, c.p, 0, 0); err != nil {
			t.Fatal(err)
		}
		got := m.lastEventQuery
		if got.UserID != c.want.UserID || got.ExcludeTriage != c.want.ExcludeTriage || got.ForwardedOnly != c.want.ForwardedOnly ||
			got.Scope.All != c.want.Scope.All || len(got.Scope.Projects) != len(c.want.Scope.Projects) || got.Limit != 100 {
			t.Errorf("%s: query = %+v", c.p.Role, got)
		}
	}
	if _, err := s.ListEvents(ctx, auth.Principal{ID: "x", Role: "guest"}, 0, 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("guest: %v", err)
	}
	if _, err := s.ListTriage(ctx, alice, "", TriageQuery{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("external triage: %v", err)
	}
}

func TestIssueCreated(t *testing.T) {
	m := newMem()
	s := newService(t, m, map[string]bool{TypeBug: true})
	ctx := context.Background()
	tk, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "x"})
	if ok, err := s.IssueCreated(ctx, tk.ID, 12, "create"); err != nil || !ok {
		t.Fatalf("IssueCreated = %v %v", ok, err)
	}
	if ok, _ := s.IssueCreated(ctx, tk.ID, 13, "create"); ok {
		t.Error("second IssueCreated overwrote the issue")
	}
	if got := m.tickets[tk.ID]; got.IssueNumber != 12 || got.Status != StatusOpen {
		t.Errorf("ticket = %+v", got)
	}
}
