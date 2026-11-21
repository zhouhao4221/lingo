package ticket

import (
	"context"
	"errors"
	"testing"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/notify"
)

func TestRejectForwardedByStatus(t *testing.T) {
	for _, c := range []struct {
		from Status
		ok   bool
	}{
		{StatusSubmitted, true}, {StatusOpen, true}, {StatusInProgress, true}, {StatusNeedInfo, true}, {StatusAbnormal, true},
		{StatusFixed, false}, {StatusReleased, false}, {StatusDeployed, false}, {StatusClosed, false},
		{StatusPendingTriage, false}, {StatusAnswered, false}, {StatusMerged, false},
	} {
		m := newMem(base(c.from))
		got, err := newService(t, m, nil).RejectForwarded(context.Background(), reviewer, 1, "不做")
		if !c.ok {
			if !errors.Is(err, ErrConflict) || m.tickets[1].Status != c.from || len(m.events) != 0 || len(m.tasks) != 0 {
				t.Errorf("%s: err %v status %s events %d tasks %d", c.from, err, m.tickets[1].Status, len(m.events), len(m.tasks))
			}
			continue
		}
		if err != nil || got.Status != StatusRejected || m.tickets[1].Status != StatusRejected {
			t.Errorf("%s: %v %+v", c.from, err, got)
		}
	}
}

// abnormal 只在没有修复记录时可驳回：已修复的工单 Issue 被删除后为 abnormal，修复记录仍在，不可驳回；
// 建单失败的 abnormal（没有 Issue）可驳回。
func TestRejectForwardedAbnormalWithFix(t *testing.T) {
	ctx := context.Background()
	fixed := base(StatusFixed)
	fixed.FixedPR = 7
	m := newMem(fixed)
	if err := newSync(t, m, t0).ApplyIssue(ctx, issue(IssueDeleted, IssueState{})); err != nil {
		t.Fatal(err)
	}
	if tk := m.tickets[1]; tk.Status != StatusAbnormal || tk.FixedPR != 7 {
		t.Fatalf("after delete: %+v", tk)
	}
	events, tasks := len(m.events), len(m.tasks)
	if _, err := newService(t, m, nil).RejectForwarded(ctx, reviewer, 1, "不做"); !errors.Is(err, ErrConflict) ||
		m.tickets[1].Status != StatusAbnormal || len(m.events) != events || len(m.tasks) != tasks {
		t.Errorf("fixed then deleted: err %v %+v", err, m.tickets[1])
	}
	for name, set := range map[string]func(*Ticket){
		"commit":  func(tk *Ticket) { tk.FixedCommit = "abc" },
		"release": func(tk *Ticket) { tk.FixedReleaseID = 3 },
		"deployed": func(tk *Ticket) {
			at := t0
			tk.DeployedAt = &at
		},
	} {
		tk := base(StatusAbnormal)
		set(&tk)
		m := newMem(tk)
		if _, err := newService(t, m, nil).RejectForwarded(ctx, reviewer, 1, "不做"); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// 关闭未修复的工单 Issue 被删除（或读取 404）：保持 closed，仍不可驳回
	m = newMem(base(StatusClosed))
	if err := newSync(t, m, t0).ApplyIssue(ctx, issue(IssueDeleted, IssueState{})); err != nil {
		t.Fatal(err)
	}
	if _, err := newService(t, m, nil).RejectForwarded(ctx, reviewer, 1, "不做"); m.tickets[1].Status != StatusClosed ||
		!errors.Is(err, ErrConflict) || len(m.tasks) != 0 {
		t.Errorf("closed then deleted: err %v %+v", err, m.tickets[1])
	}
	// 处理中的工单 Issue 被删除：abnormal，仍可驳回
	m = newMem(base(StatusInProgress))
	if err := newSync(t, m, t0).ApplyIssue(ctx, issue(IssueDeleted, IssueState{})); err != nil {
		t.Fatal(err)
	}
	if got, err := newService(t, m, nil).RejectForwarded(ctx, reviewer, 1, "不做"); err != nil || got.Status != StatusRejected {
		t.Errorf("in progress then deleted: %v %+v", err, got)
	}
	// 存量：早期版本把已关闭工单在 Issue 删除后改成了 abnormal。按最近一次进入 abnormal 的状态历史判断
	entered := func(prev Status) History {
		return History{TicketID: 1, Source: "gitea", Kind: string(IssueDeleted),
			Payload: map[string]any{"status": string(StatusAbnormal), "prev_status": string(prev)}}
	}
	stale := History{TicketID: 1, Source: "gitea", Kind: string(IssueLabeled),
		Payload: map[string]any{"status": string(StatusAbnormal), "prev_status": string(StatusAbnormal), "stale": true}}
	recovered := History{TicketID: 1, Source: "gitea", Kind: string(IssueSynced),
		Payload: map[string]any{"status": string(StatusInProgress), "prev_status": string(StatusAbnormal)}}
	other := History{TicketID: 2, Source: "gitea", Kind: string(IssueDeleted),
		Payload: map[string]any{"status": string(StatusAbnormal), "prev_status": string(StatusClosed)}}
	for _, c := range []struct {
		name    string
		history []History
		ok      bool
	}{
		{"legacy closed", []History{entered(StatusClosed)}, false},
		{"legacy closed then stale event", []History{entered(StatusClosed), stale}, false},
		{"in progress", []History{entered(StatusInProgress)}, true},
		{"closed, recovered, deleted again while in progress", []History{entered(StatusClosed), recovered, entered(StatusInProgress)}, true},
		{"create failed", []History{{TicketID: 1, Source: "system", Kind: "issue.create_failed"}}, true},
		{"other ticket's history", []History{other, entered(StatusOpen)}, true},
		{"no history", nil, true},
	} {
		m := newMem(base(StatusAbnormal))
		m.history = append(m.history, c.history...)
		got, err := newService(t, m, nil).RejectForwarded(ctx, reviewer, 1, "不做")
		if c.ok != (err == nil) {
			t.Errorf("%s: %v %+v", c.name, err, got)
		}
		if !c.ok && (!errors.Is(err, ErrConflict) || m.tickets[1].Status != StatusAbnormal || len(m.tasks) != 0 || len(m.events) != 0) {
			t.Errorf("%s: err %v %+v", c.name, err, m.tickets[1])
		}
	}
	// 建单失败：没有 Issue、没有修复记录，可驳回且不入队关闭
	failed := base(StatusAbnormal)
	failed.IssueNumber = 0
	m = newMem(failed)
	if got, err := newService(t, m, nil).RejectForwarded(ctx, reviewer, 1, "不做"); err != nil || got.Status != StatusRejected || len(m.tasks) != 0 {
		t.Errorf("create failed: %v %+v tasks %v", err, got, m.tasks)
	}
}

func TestRejectForwardedEffectsAndIdempotent(t *testing.T) {
	tk := base(StatusInProgress)
	m := newMem(tk)
	m.notes = []Comment{{TicketID: 1, Source: "user", Body: "补充"}}
	s := newService(t, m, nil)
	ctx := context.Background()
	got, err := s.RejectForwarded(ctx, admin, 1, "  需求不成立  ")
	if err != nil || got.Status != StatusRejected || got.IssueNumber != 12 || got.Repo != "demo/api" {
		t.Fatalf("reject: %v %+v", err, got)
	}
	if len(m.events) != 1 || m.events[0].Kind != notify.TicketStatus || m.events[0].PrevStatus != "in_progress" ||
		m.events[0].Status != "rejected" || m.events[0].Body != "需求不成立" || m.events[0].ActorID != "root" {
		t.Errorf("events = %+v", m.events)
	}
	if h := m.history[len(m.history)-1]; h.Kind != "ticket.rejected" || h.ActorID != "root" || h.Payload["note"] != "需求不成立" ||
		h.Payload["prev_status"] != "in_progress" || h.Payload["issue"] != int64(12) {
		t.Errorf("history = %+v", h)
	}
	task, ok := m.tasks["close_issue:ticket:1"]
	if !ok || len(m.tasks) != 1 || task.Op != OpCloseIssue || task.TicketID != 1 || task.Payload["issue"] != int64(12) {
		t.Errorf("tasks = %+v", m.tasks)
	}
	if len(m.notes) != 1 {
		t.Errorf("comments changed: %+v", m.notes)
	}
	// 重复驳回（如接入方超时重试）：返回当前工单，不重复留痕、通知或入队
	nh := len(m.history)
	again, err := s.RejectForwarded(ctx, reviewer, 1, "另一个原因")
	if err != nil || again.Status != StatusRejected || len(m.events) != 1 || len(m.history) != nh || len(m.tasks) != 1 {
		t.Errorf("repeat: %v %+v events %d history %d tasks %d", err, again, len(m.events), len(m.history), len(m.tasks))
	}
}

func TestRejectForwardedAccess(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		p    auth.Principal
		why  string
		want error
	}{
		{"external", auth.Principal{ID: "u1", Role: auth.RoleExternal, Projects: []string{"demo"}}, "x", ErrForbidden},
		{"agent", auth.Principal{ID: "g1", Role: auth.RoleAgent, Projects: []string{"demo"}}, "x", ErrForbidden},
		{"other project", auth.Principal{ID: "r2", Role: auth.RoleReviewer, Projects: []string{"other"}}, "x", ErrNotFound},
		{"empty reason", reviewer, "  ", ErrInvalid},
	} {
		m := newMem(base(StatusOpen))
		if _, err := newService(t, m, nil).RejectForwarded(ctx, c.p, 1, c.why); !errors.Is(err, c.want) || m.tickets[1].Status != StatusOpen {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := newService(t, newMem(), nil).RejectForwarded(ctx, reviewer, 9, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestRejectForwardedWithoutIssue(t *testing.T) {
	tk := base(StatusSubmitted)
	tk.IssueNumber = 0
	m := newMem(tk)
	s := newService(t, m, nil)
	ctx := context.Background()
	if _, err := s.RejectForwarded(ctx, reviewer, 1, "重复"); err != nil || len(m.tasks) != 0 {
		t.Fatalf("reject: %v tasks %+v", err, m.tasks)
	}
	if c, err := s.IssueClose(ctx, reviewer, 1); err != nil || c.State != CloseNone || c.Number != 0 {
		t.Errorf("close state: %v %+v", err, c)
	}
	if _, err := s.RetryIssueClose(ctx, reviewer, 1); !errors.Is(err, ErrConflict) {
		t.Errorf("retry without issue: %v", err)
	}
	// 建单任务已在 Gitea 建好 Issue 才回来记录：补记关联并入队关闭，工单保持驳回、不发状态事件
	ne := len(m.events)
	linked, err := s.IssueCreated(ctx, 1, 40, "create", nil)
	if err != nil || linked || m.tickets[1].Status != StatusRejected || m.tickets[1].IssueNumber != 40 || len(m.events) != ne {
		t.Fatalf("issue created: %v linked %v %+v", err, linked, m.tickets[1])
	}
	if _, ok := m.tasks["close_issue:ticket:1"]; !ok {
		t.Errorf("close not enqueued: %+v", m.tasks)
	}
	// 重复回报（如查重路径）不重复入队
	if _, err := s.IssueCreated(ctx, 1, 40, "dedup", nil); err != nil || len(m.tasks) != 1 {
		t.Errorf("repeat: %v %+v", err, m.tasks)
	}
}

func TestIssueCloseStateAndRetry(t *testing.T) {
	m := newMem(base(StatusOpen))
	m.states = map[string]*TaskState{}
	s := newService(t, m, nil)
	ctx := context.Background()
	if c, err := s.IssueClose(ctx, reviewer, 1); err != nil || c.State != CloseNone || c.Attempt != 0 {
		t.Fatalf("before reject: %v %+v", err, c)
	}
	if _, err := s.RejectForwarded(ctx, reviewer, 1, "x"); err != nil {
		t.Fatal(err)
	}
	c, err := s.IssueClose(ctx, reviewer, 1)
	if err != nil || c.State != ClosePending || c.Attempt != 1 || c.Repo != "demo/api" || c.Number != 12 {
		t.Fatalf("pending: %v %+v", err, c)
	}
	// 排队中：重试不重复入队
	if c, err := s.RetryIssueClose(ctx, reviewer, 1); err != nil || c.State != ClosePending || len(m.tasks) != 1 {
		t.Errorf("retry pending: %v %+v tasks %d", err, c, len(m.tasks))
	}
	m.states["close_issue:ticket:1"] = &TaskState{Status: CloseFailed, Failures: 10, LastError: "close issue: 403 forbidden"}
	if c, err := s.IssueClose(ctx, admin, 1); err != nil || c.State != CloseFailed || c.Failures != 10 || c.LastError == "" {
		t.Errorf("failed: %v %+v", err, c)
	}
	nh := len(m.history)
	c, err = s.RetryIssueClose(ctx, admin, 1)
	if err != nil || c.State != ClosePending || c.Attempt != 2 || c.LastError != "" {
		t.Fatalf("retry: %v %+v", err, c)
	}
	if _, ok := m.tasks["close_issue:ticket:1:retry:1"]; !ok || len(m.tasks) != 2 || len(m.history) != nh+1 || m.history[nh].Kind != "issue.close_retry" {
		t.Errorf("retry task: %+v history %+v", m.tasks, m.history[nh:])
	}
	if c, err := s.RetryIssueClose(ctx, admin, 1); err != nil || c.Attempt != 2 || len(m.tasks) != 2 {
		t.Errorf("double retry: %v %+v tasks %d", err, c, len(m.tasks))
	}
	m.states["close_issue:ticket:1:retry:1"] = &TaskState{Status: CloseDone}
	if c, err := s.RetryIssueClose(ctx, admin, 1); err != nil || c.State != CloseDone || len(m.tasks) != 2 {
		t.Errorf("retry done: %v %+v", err, c)
	}
	if _, err := s.IssueClose(ctx, auth.Principal{ID: "u1", Role: auth.RoleExternal, Projects: []string{"demo"}}, 1); !errors.Is(err, ErrForbidden) {
		t.Errorf("external: %v", err)
	}
}

// 本地驳回结论不被 Gitea 回流覆盖：灵问自己关闭 Issue 的回流、研发重开、删除、改标签、对账快照、PR 合并都只留痕。
func TestSyncKeepsLocalRejection(t *testing.T) {
	ctx := context.Background()
	for _, u := range []IssueUpdate{
		issue(IssueClosed, IssueState{State: "closed"}),
		issue(IssueReopened, IssueState{State: "open"}),
		issue(IssueDeleted, IssueState{}),
		issue(IssueAssigned, IssueState{State: "open", Assignees: []string{"dev"}}),
		issue(IssueLabeled, IssueState{State: "open", Labels: []string{"lingo/review/need-info"}}),
		issue(IssueSynced, IssueState{State: "open", Assignees: []string{"dev"}}),
		issue(IssueSynced, IssueState{State: "closed"}),
		issue(IssueMilestoned, IssueState{State: "open", Milestone: "v2"}),
	} {
		m := newMem(base(StatusRejected))
		if err := newSync(t, m, t0).ApplyIssue(ctx, u); err != nil {
			t.Fatalf("%s: %v", u.Change, err)
		}
		if tk := m.tickets[1]; tk.Status != StatusRejected || tk.Milestone != "" || len(m.events) != 0 || len(m.history) != 1 {
			t.Errorf("%s: %+v events %+v", u.Change, tk, m.events)
		}
	}
	m := newMem(base(StatusRejected))
	if err := newSync(t, m, t0).ApplyPull(ctx, PullUpdate{Change: PullMerged, Repo: "demo/api", Number: 3,
		Refs: []IssueRef{{Repo: "demo/api", Number: 12, Closes: true}}}); err != nil {
		t.Fatal(err)
	}
	if tk := m.tickets[1]; tk.Status != StatusRejected || tk.FixedPR != 0 || len(m.events) != 0 {
		t.Errorf("pull merged: %+v", tk)
	}
}

func TestLinkByMarkerOnRejected(t *testing.T) {
	tk := base(StatusRejected)
	tk.IssueNumber = 0
	m := newMem(tk)
	u := issue(IssueOpened, IssueState{State: "open"})
	u.MarkerTicketID = 1
	if err := newSync(t, m, t0).ApplyIssue(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if got := m.tickets[1]; got.Status != StatusRejected || got.IssueNumber != 12 || len(m.events) != 0 {
		t.Errorf("ticket = %+v events %+v", got, m.events)
	}
	if _, ok := m.tasks["close_issue:ticket:1"]; !ok {
		t.Errorf("close not enqueued: %+v", m.tasks)
	}
}

// 已有契约：reviewer / admin 在任何状态都能写对外评论（合并后的工单除外，评论写到目标工单）；
// 已转出且有 Issue 时同步到 Issue，驳回 / 未转出的只留在灵问。
func TestStaffCanCommentAnytime(t *testing.T) {
	ctx := context.Background()
	for _, st := range []Status{StatusPendingTriage, StatusAnswered, StatusRejected, StatusSubmitted, StatusOpen, StatusInProgress,
		StatusNeedInfo, StatusFixed, StatusReleased, StatusDeployed, StatusClosed, StatusAbnormal} {
		for _, p := range []auth.Principal{reviewer, admin} {
			m := newMem(base(st))
			c, err := newService(t, m, nil).AddComment(ctx, p, 1, "后台说明", nil)
			if err != nil || c.Source != "staff" || !c.Public || len(m.notes) != 1 {
				t.Errorf("%s by %s: %v %+v", st, p.Role, err, c)
				continue
			}
			_, synced := m.tasks["create_comment:comment:1"]
			if want := st.forwarded() && st != StatusAbnormal; synced != want {
				t.Errorf("%s by %s: synced to issue = %v, want %v", st, p.Role, synced, want)
			}
			if m.tickets[1].Status != st {
				t.Errorf("%s by %s: status changed to %s", st, p.Role, m.tickets[1].Status)
			}
		}
	}
	m := newMem(base(StatusMerged))
	if _, err := newService(t, m, nil).AddComment(ctx, admin, 1, "x", nil); !errors.Is(err, ErrConflict) {
		t.Errorf("merged: %v", err)
	}
}
