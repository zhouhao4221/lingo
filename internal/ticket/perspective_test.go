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

// allowAll 模拟接入方放行所有视角补充（AuthorizePerspective）。
func allowAll(context.Context, auth.Principal, *Ticket, bool) error { return nil }

func newPerspectiveService(t *testing.T, m *memStore, required map[string][]string) *Service {
	t.Helper()
	s, err := NewService(ServiceOptions{Store: m, Now: func() time.Time { return t0 },
		RequiredPerspectives: func(key string) []string { return required[key] }, AuthorizePerspective: allowAll})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAddPerspectiveRules(t *testing.T) {
	ctx := context.Background()
	m := newMem(
		Ticket{ID: 1, ProjectKey: "demo", Type: TypeFeature, Status: StatusPendingTriage, UserID: "u1", Body: "要导出"},
		Ticket{ID: 2, ProjectKey: "demo", Type: TypeBug, Status: StatusPendingTriage, UserID: "u1"},
		Ticket{ID: 3, ProjectKey: "demo", Type: TypeFeature, Status: StatusClosed, UserID: "u1"},
		Ticket{ID: 4, ProjectKey: "other", Type: TypeFeature, Status: StatusOpen, UserID: "u1"},
		Ticket{ID: 5, ProjectKey: "demo", Type: TypeFeature, Status: StatusOpen, UserID: "u1", Repo: "demo/api", IssueNumber: 9},
		Ticket{ID: 6, ProjectKey: "demo", Type: TypeFeature, Status: StatusFixed, UserID: "u1", Repo: "demo/api", IssueNumber: 10},
	)
	s := newPerspectiveService(t, m, nil)
	biz := auth.Principal{ID: "b1", Name: "张三", Role: auth.RoleExternal, Projects: []string{"demo"}, Perspective: PerspectiveBusiness}

	// external 可补充项目内他人的需求（权限由接入方判断）；未转出不入队
	p, err := s.AddPerspective(ctx, biz, 1, "  影响 30 家门店  ")
	if err != nil || p.Perspective != PerspectiveBusiness || p.Body != "影响 30 家门店" || p.AuthorName != "张三" {
		t.Fatalf("add = %+v, %v", p, err)
	}
	if len(m.tasks) != 0 {
		t.Errorf("pending ticket enqueued: %+v", m.tasks)
	}
	e := m.events[len(m.events)-1]
	if e.Kind != notify.TicketPerspectiveAdded || e.Body != "业务方 · 张三 补充了内容" || len(e.Perspectives) != 1 || e.ActorID != "b1" || e.UserID != "u1" {
		t.Errorf("event = %+v", e)
	}

	for _, c := range []struct {
		name string
		p    auth.Principal
		id   int64
		body string
		want error
	}{
		{"bad perspective", auth.Principal{ID: "x", Role: auth.RoleExternal, Perspective: "boss"}, 1, "a", ErrInvalid},
		{"empty", biz, 1, "  ", ErrInvalid},
		{"too long", biz, 1, strings.Repeat("字", MaxPerspectiveRunes+1), ErrInvalid},
		{"bug ticket", biz, 2, "a", ErrInvalid},
		{"closed", biz, 3, "a", ErrConflict},
		{"other project", biz, 4, "a", ErrNotFound},
		{"missing", biz, 99, "a", ErrNotFound},
		{"no id", auth.Principal{Role: auth.RoleExternal}, 1, "a", ErrForbidden},
	} {
		if _, err := s.AddPerspective(ctx, c.p, c.id, c.body); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := s.AddPerspective(ctx, biz, 1, strings.Repeat("字", MaxPerspectiveRunes)); err != nil {
		t.Errorf("10000 runes rejected: %v", err)
	}

	// 已转出（含已修复）入队同步任务，Key 按补充唯一；空视角按 user
	p5, err := s.AddPerspective(ctx, auth.Principal{ID: "u9", Role: auth.RoleExternal}, 5, "我也要")
	if err != nil || p5.Perspective != PerspectiveUser {
		t.Fatalf("add forwarded = %+v, %v", p5, err)
	}
	if _, err := s.AddPerspective(ctx, biz, 6, "补充"); err != nil {
		t.Fatal(err)
	}
	if len(m.tasks) != 2 {
		t.Errorf("tasks = %+v", m.tasks)
	}
	for k, task := range m.tasks {
		if task.Op != OpSyncPerspectives || !strings.HasPrefix(k, OpSyncPerspectives+":perspective:") {
			t.Errorf("task %s = %+v", k, task)
		}
	}
}

func TestListPerspectivesGroupsAndMissing(t *testing.T) {
	ctx := context.Background()
	m := newMem(Ticket{ID: 1, ProjectKey: "demo", Type: TypeFeature, Status: StatusPendingTriage, UserID: "u1", UserName: "王五",
		UserPerspective: PerspectiveUser, Fields: map[string][]string{"what": {"导出"}, "why": {"对账"}}})
	s := newPerspectiveService(t, m, map[string][]string{"demo": {PerspectiveLeader, PerspectiveBusiness, "bogus"}})
	lead := auth.Principal{ID: "l1", Role: auth.RoleExternal, Perspective: PerspectiveLeader}
	set, err := s.ListPerspectives(ctx, lead, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Items) != 1 || !set.Items[0].Original || set.Items[0].Body != "what：导出\nwhy：对账" || set.Items[0].AuthorName != "王五" {
		t.Errorf("first = %+v", set.Items)
	}
	if strings.Join(set.Missing, ",") != "business,leader" {
		t.Errorf("missing = %v", set.Missing)
	}
	if _, err := s.AddPerspective(ctx, lead, 1, "本月做"); err != nil {
		t.Fatal(err)
	}
	set, _ = s.ListPerspectives(ctx, lead, 1)
	if strings.Join(set.Missing, ",") != "business" || strings.Join(set.Present(), ",") != "user,leader" {
		t.Errorf("after add: missing %v present %v", set.Missing, set.Present())
	}
	if _, err := s.ListPerspectives(ctx, auth.Principal{ID: "x", Role: auth.RoleReviewer, Projects: []string{"other"}}, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("out of project: %v", err)
	}
}

func TestForwardEmitsPerspectiveMissing(t *testing.T) {
	ctx := context.Background()
	m := newMem(
		Ticket{ID: 1, ProjectKey: "demo", Type: TypeFeature, Status: StatusPendingTriage, UserID: "u1"},
		Ticket{ID: 2, ProjectKey: "demo", Type: TypeConsult, Status: StatusPendingTriage, UserID: "u1"},
	)
	s := newPerspectiveService(t, m, map[string][]string{"demo": {PerspectiveLeader}})
	if _, err := s.Forward(ctx, reviewer, 1, ForwardInput{}); err != nil {
		t.Fatal(err)
	}
	var missing []notify.Event
	for _, e := range m.events {
		if e.Kind == notify.TicketPerspectiveMissing {
			missing = append(missing, e)
		}
	}
	if len(missing) != 1 || missing[0].Body != "缺少：决策意见" || missing[0].Perspectives[0] != PerspectiveLeader {
		t.Fatalf("missing events = %+v", missing)
	}
	// 咨询转出时改为需求：同样检查（首段由工单派生）
	if _, err := s.Forward(ctx, reviewer, 2, ForwardInput{Type: TypeFeature}); err != nil {
		t.Fatal(err)
	}
	if n := len(kindsOf(m.events, notify.TicketPerspectiveMissing)); n != 2 {
		t.Errorf("missing after type change = %d", n)
	}
}

func TestCreateValidatesPerspective(t *testing.T) {
	m := newMem()
	s := newPerspectiveService(t, m, map[string][]string{"demo": {PerspectiveBusiness}})
	ctx := context.Background()
	if _, err := s.Create(ctx, auth.Principal{ID: "u1", Role: auth.RoleExternal, Perspective: "boss"}, NewTicket{Project: "demo", Type: TypeFeature, Body: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad perspective: %v", err)
	}
	tk, err := s.Create(ctx, auth.Principal{ID: "u1", Role: auth.RoleExternal, Perspective: PerspectiveBusiness}, NewTicket{Project: "demo", Type: TypeFeature, Body: "x"})
	if err != nil || tk.UserPerspective != PerspectiveBusiness {
		t.Fatalf("create = %+v, %v", tk, err)
	}
	if n := len(kindsOf(m.events, notify.TicketPerspectiveMissing)); n != 0 {
		t.Errorf("pending ticket emitted missing: %d", n)
	}
}

func kindsOf(es []notify.Event, k notify.Kind) []notify.Event {
	var out []notify.Event
	for _, e := range es {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func TestPerspectiveAuthorization(t *testing.T) {
	ctx := context.Background()
	m := newMem(
		Ticket{ID: 1, ProjectKey: "demo", Type: TypeFeature, Status: StatusPendingTriage, UserID: "u1"},
		Ticket{ID: 2, ProjectKey: "demo", Type: TypeFeature, Status: StatusOpen, UserID: "u1"},
	)
	// 未配置 AuthorizePerspective：按工单可见范围
	s, _ := NewService(ServiceOptions{Store: m, Now: func() time.Time { return t0 }})
	for _, c := range []struct {
		name string
		p    auth.Principal
		id   int64
		want error
	}{
		{"owner", alice, 1, nil},
		{"other external", bob, 1, ErrNotFound},
		{"agent pending", agent, 1, ErrNotFound},
		{"agent forwarded", agent, 2, nil},
		{"reviewer", reviewer, 1, nil},
	} {
		if _, err := s.AddPerspective(ctx, c.p, c.id, "x"); !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
			t.Errorf("add %s: %v", c.name, err)
		}
		if _, err := s.ListPerspectives(ctx, c.p, c.id); !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
			t.Errorf("list %s: %v", c.name, err)
		}
	}
	// 配置后由接入方决定，write 区分补充与查看；拒绝时不写入
	var writes []bool
	s2, _ := NewService(ServiceOptions{Store: m, Now: func() time.Time { return t0 },
		AuthorizePerspective: func(_ context.Context, p auth.Principal, tk *Ticket, write bool) error {
			writes = append(writes, write)
			if write && p.Perspective != PerspectiveLeader {
				return ErrForbidden
			}
			return nil
		}})
	n := len(m.perspectives)
	if _, err := s2.AddPerspective(ctx, bob, 1, "x"); !errors.Is(err, ErrForbidden) || len(m.perspectives) != n {
		t.Errorf("denied add: %v", err)
	}
	if _, err := s2.ListPerspectives(ctx, bob, 1); err != nil {
		t.Errorf("allowed list: %v", err)
	}
	lead := auth.Principal{ID: "l1", Role: auth.RoleExternal, Perspective: PerspectiveLeader}
	if _, err := s2.AddPerspective(ctx, lead, 1, "x"); err != nil {
		t.Errorf("allowed add: %v", err)
	}
	if len(writes) != 3 || !writes[0] || writes[1] || !writes[2] {
		t.Errorf("writes = %v", writes)
	}
}

func TestAbnormalWithIssueEnqueues(t *testing.T) {
	m := newMem(
		Ticket{ID: 1, ProjectKey: "demo", Type: TypeFeature, Status: StatusAbnormal, UserID: "u1", Repo: "demo/api", IssueNumber: 5},
		Ticket{ID: 2, ProjectKey: "demo", Type: TypeFeature, Status: StatusAbnormal, UserID: "u1"},
	)
	s := newPerspectiveService(t, m, nil)
	for _, id := range []int64{1, 2} {
		if _, err := s.AddPerspective(context.Background(), alice, id, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.tasks) != 1 {
		t.Errorf("tasks = %+v", m.tasks)
	}
}

func TestForwardWithoutRequiredEmitsNoMissing(t *testing.T) {
	m := newMem(Ticket{ID: 1, ProjectKey: "demo", Type: TypeFeature, Status: StatusPendingTriage, UserID: "u1"})
	s := newPerspectiveService(t, m, nil)
	if _, err := s.Forward(context.Background(), reviewer, 1, ForwardInput{}); err != nil {
		t.Fatal(err)
	}
	if n := len(kindsOf(m.events, notify.TicketPerspectiveMissing)); n != 0 {
		t.Errorf("missing events without required perspectives: %d", n)
	}
	set, _ := s.ListPerspectives(context.Background(), reviewer, 1)
	if len(set.Missing) != 0 {
		t.Errorf("missing = %v", set.Missing)
	}
}
