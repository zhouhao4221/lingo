package ticket

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
)

// memStore 是内存中的 Store：InTx 失败时回滚到调用前的快照。
type memStore struct {
	tickets  map[int64]*Ticket
	comments map[int64]Comment
	history  []History
	events   []notify.Event

	projects map[string]*Project
	repos    map[int64][]RepoRef // project → repos
	auto     map[string]bool     // "project:type"
	notes    []Comment
	tasks    map[string]Task
	states   map[string]*TaskState // 任务执行状态（测试设置）；有任务而未设置时为 pending
	reminded map[int64]time.Time

	perspectives []Perspective
	withheld     map[int64]bool // 评论 ID → 已标记驳回后未发出
	sending      map[int64]bool // 评论 ID → 持有发送租约

	lastEventQuery EventQuery
	lastSearch     *SearchQuery
	lastVisible    *VisibleQuery
	attachStub     []Attachment // TicketAttachments 的返回
}

func newMem(ts ...Ticket) *memStore {
	m := &memStore{tickets: map[int64]*Ticket{}, comments: map[int64]Comment{}, auto: map[string]bool{}, tasks: map[string]Task{}, reminded: map[int64]time.Time{},
		projects: map[string]*Project{"demo": {ID: 1, Key: "demo", DefaultRepo: &RepoRef{ID: 10, Name: "demo/api"}}, "empty": {ID: 2, Key: "empty"}},
		repos:    map[int64][]RepoRef{1: {{ID: 10, Name: "demo/api"}, {ID: 11, Name: "demo/web"}}}}
	for _, t := range ts {
		m.tickets[t.ID] = &t
	}
	return m
}

func (m *memStore) InTx(ctx context.Context, fn func(Tx) error) error {
	saved := map[int64]Ticket{}
	for id, t := range m.tickets {
		saved[id] = *t
	}
	nh, ne, nn, np := len(m.history), len(m.events), len(m.notes), len(m.perspectives)
	if err := fn(m); err != nil {
		for id := range m.tickets {
			if t, ok := saved[id]; ok {
				*m.tickets[id] = t
			} else {
				delete(m.tickets, id)
			}
		}
		m.history, m.events, m.notes, m.perspectives = m.history[:nh], m.events[:ne], m.notes[:nn], m.perspectives[:np]
		return err
	}
	return nil
}

func (m *memStore) Get(_ context.Context, id int64) (*Ticket, error) {
	if t, ok := m.tickets[id]; ok {
		c := *t
		return &c, nil
	}
	return nil, ErrNotFound
}

func (m *memStore) GetMany(ctx context.Context, ids []int64) ([]*Ticket, error) {
	var out []*Ticket
	for _, id := range ids {
		if t, err := m.Get(ctx, id); err == nil {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memStore) Insert(_ context.Context, t *Ticket) error {
	t.ID = int64(len(m.tickets) + 1)
	c := *t
	m.tickets[t.ID] = &c
	return nil
}

func (m *memStore) Project(_ context.Context, key string) (*Project, error) {
	if p, ok := m.projects[key]; ok {
		return p, nil
	}
	return nil, ErrNotFound
}

func (m *memStore) Repo(_ context.Context, pid int64, name string) (*RepoRef, error) {
	for _, r := range m.repos[pid] {
		if strings.EqualFold(r.Name, name) {
			return &r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *memStore) AutoForward(_ context.Context, pid int64, typ string) (bool, bool, error) {
	on, ok := m.auto[fmt.Sprint(pid)+":"+typ]
	return on, ok, nil
}

func (m *memStore) SetAutoForward(_ context.Context, pid int64, typ string, on *bool, _ string) error {
	k := fmt.Sprint(pid) + ":" + typ
	if on == nil {
		delete(m.auto, k)
	} else {
		m.auto[k] = *on
	}
	return nil
}

func (m *memStore) LockOverdue(_ context.Context, _ Scope, before time.Time, limit int) ([]*Ticket, error) {
	var out []*Ticket
	for id := int64(1); id <= int64(len(m.tickets)); id++ {
		t, ok := m.tickets[id]
		if !ok || t.Status != StatusPendingTriage || t.TriageSince == nil {
			continue
		}
		last := *t.TriageSince
		if r, ok := m.reminded[id]; ok {
			last = r
		}
		if last.Before(before) && len(out) < limit {
			c := *t
			out = append(out, &c)
		}
	}
	return out, nil
}

func (m *memStore) MarkReminded(_ context.Context, id int64, at time.Time) error {
	m.reminded[id] = at
	return nil
}

func (m *memStore) ListTriage(context.Context, TriageQuery) ([]*Ticket, error) { return nil, nil }
func (m *memStore) TriageCounts(context.Context, Scope, time.Time) ([]TriageCount, error) {
	return nil, nil
}
func (m *memStore) ListMine(context.Context, MineQuery) ([]*Ticket, error) { return nil, nil }
func (m *memStore) ListVisible(_ context.Context, q VisibleQuery) ([]*Ticket, error) {
	m.lastVisible = &q
	return nil, nil
}
func (m *memStore) Search(_ context.Context, q SearchQuery) ([]*Ticket, error) {
	m.lastSearch = &q
	var out []*Ticket
	for _, t := range m.matching(q) {
		if q.BeforeID > 0 && t.ID >= q.BeforeID {
			continue
		}
		if len(out) == q.Limit {
			break
		}
		out = append(out, t)
	}
	return out, nil
}
func (m *memStore) Stats(_ context.Context, q SearchQuery) ([]StatRow, error) {
	m.lastSearch = &q
	var out []StatRow
	for _, t := range m.matching(q) { // 每张一行，调用方只求和
		out = append(out, StatRow{ProjectKey: t.ProjectKey, Type: t.Type, Status: string(t.Status), Count: 1})
	}
	return out, nil
}

// matching 按 store.searchWhere 的语义在内存中过滤（范围、提交人、已转出、仓库、类型、状态、关键词），ID 降序。
func (m *memStore) matching(q SearchQuery) []*Ticket {
	var out []*Ticket
	for _, t := range m.tickets {
		switch {
		case !q.Scope.All && !slices.Contains(q.Scope.Projects, t.ProjectKey),
			q.UserID != "" && t.UserID != q.UserID,
			len(q.UserIDs) > 0 && !slices.Contains(q.UserIDs, t.UserID),
			q.ForwardedOnly && !t.Status.forwarded(),
			q.Repo != "" && !strings.EqualFold(q.Repo, t.Repo),
			q.Type != "" && t.Type != q.Type,
			len(q.Statuses) > 0 && !slices.Contains(q.Statuses, string(t.Status)):
			continue
		}
		hit := true
		for _, k := range q.Keywords {
			k = strings.ToLower(k)
			hit = hit && (strings.Contains(strings.ToLower(t.Title), k) || strings.Contains(strings.ToLower(t.Body), k))
		}
		if hit {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b *Ticket) int { return int(b.ID - a.ID) })
	return out
}
func (m *memStore) Events(_ context.Context, q EventQuery) ([]notify.Event, error) {
	m.lastEventQuery = q
	return nil, nil
}
func (m *memStore) AutoForwardOverrides(context.Context, []string) (map[string]map[string]bool, error) {
	return nil, nil
}

func (m *memStore) TicketAttachments(context.Context, int64, bool, bool) ([]Attachment, error) {
	return m.attachStub, nil
}

func (m *memStore) Comments(context.Context, int64, bool) ([]Comment, error) { return nil, nil }

func (m *memStore) AddPerspective(_ context.Context, p Perspective) (int64, error) {
	p.ID = int64(len(m.perspectives) + 1)
	m.perspectives = append(m.perspectives, p)
	return p.ID, nil
}

func (m *memStore) Perspectives(_ context.Context, id int64) ([]Perspective, error) {
	var out []Perspective
	for _, p := range m.perspectives {
		if p.TicketID == id {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *memStore) AddComment(_ context.Context, c Comment) (int64, error) {
	m.notes = append(m.notes, c)
	return int64(len(m.notes)), nil
}

func (m *memStore) MarkCommentsForwarded(context.Context, int64, []int64) error { return nil }

func (m *memStore) AttachmentIDs(_ context.Context, id int64) ([]string, error) {
	var out []string
	if t, ok := m.tickets[id]; ok {
		for _, a := range t.Attachments {
			out = append(out, a.ID)
		}
	}
	for _, c := range m.notes {
		if c.TicketID == id {
			for _, a := range c.Attachments {
				out = append(out, a.ID)
			}
		}
	}
	return out, nil
}
func (m *memStore) Enqueue(_ context.Context, t Task) error {
	if _, ok := m.tasks[t.Key]; !ok {
		m.tasks[t.Key] = t
	}
	return nil
}

func (m *memStore) TaskState(_ context.Context, key string) (*TaskState, error) {
	if _, ok := m.tasks[key]; !ok {
		return nil, nil
	}
	if st, ok := m.states[key]; ok {
		c := *st
		return &c, nil
	}
	return &TaskState{Key: key, Status: "pending"}, nil
}

func (m *memStore) HasIssue(ctx context.Context, repo string, n int64) (bool, error) {
	_, err := m.LockByIssue(ctx, repo, n)
	return err == nil, nil
}

func (m *memStore) LockByIssue(_ context.Context, repo string, n int64) (*Ticket, error) {
	for _, t := range m.tickets {
		if strings.EqualFold(t.Repo, repo) && t.IssueNumber == n && n != 0 {
			c := *t
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *memStore) LockByID(_ context.Context, id int64) (*Ticket, error) {
	if t, ok := m.tickets[id]; ok {
		c := *t
		return &c, nil
	}
	return nil, ErrNotFound
}

func (m *memStore) Update(_ context.Context, t *Ticket) error { *m.tickets[t.ID] = *t; return nil }
func (m *memStore) RepointMerged(_ context.Context, from, to int64) error {
	for _, t := range m.tickets {
		if t.MergedInto == from {
			t.MergedInto = to
		}
	}
	return nil
}

func (m *memStore) PendingTask(_ context.Context, id int64, op string) (bool, error) {
	for _, t := range m.tasks {
		if t.TicketID == id && t.Op == op {
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) SeenPull(_ context.Context, id int64, kind, pull string) (bool, error) {
	for _, h := range m.history {
		if h.TicketID == id && h.Kind == kind && h.Payload["pull"] == pull {
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) RejectionNote(_ context.Context, id int64) (string, string, error) {
	for i := len(m.history) - 1; i >= 0; i-- {
		if h := m.history[i]; h.TicketID == id && h.Kind == "ticket.rejected" {
			reason, _ := h.Payload["note"].(string)
			by, _ := h.Payload["rejected_by"].(string)
			return reason, by, nil
		}
	}
	return "", "", nil
}

func (m *memStore) ClaimCommentSend(_ context.Context, ticketID, commentID int64, _ time.Duration) (bool, error) {
	if commentID < 1 || commentID > int64(len(m.notes)) || m.notes[commentID-1].TicketID != ticketID || m.withheld[commentID] {
		return false, nil
	}
	if m.sending == nil {
		m.sending = map[int64]bool{}
	}
	m.sending[commentID] = true
	return true, nil
}

func (m *memStore) ReleaseCommentSend(_ context.Context, _, commentID int64) error {
	delete(m.sending, commentID)
	return nil
}

func (m *memStore) CommentSending(_ context.Context, ticketID int64) (bool, error) {
	for id := range m.sending {
		if m.notes[id-1].TicketID == ticketID {
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) MarkCommentWithheld(_ context.Context, ticketID, commentID int64) (bool, error) {
	if commentID < 1 || commentID > int64(len(m.notes)) || m.notes[commentID-1].TicketID != ticketID || m.withheld[commentID] {
		return false, nil
	}
	if m.withheld == nil {
		m.withheld = map[int64]bool{}
	}
	m.withheld[commentID] = true
	delete(m.sending, commentID)
	return true, nil
}

func (m *memStore) AbnormalFrom(_ context.Context, id int64) (Status, error) {
	for i := len(m.history) - 1; i >= 0; i-- {
		h := m.history[i]
		switch {
		case h.TicketID != id:
		case h.Kind == "issue.create_failed":
			return StatusSubmitted, nil
		case h.Payload["status"] == string(StatusAbnormal) && h.Payload["prev_status"] != string(StatusAbnormal):
			p, _ := h.Payload["prev_status"].(string)
			return Status(p), nil
		}
	}
	return "", nil
}

func (m *memStore) AddHistory(_ context.Context, h History) error {
	m.history = append(m.history, h)
	return nil
}

func (m *memStore) SaveGiteaComment(_ context.Context, c Comment) (*Comment, error) {
	prev, ok := m.comments[c.GiteaID]
	m.comments[c.GiteaID] = c
	if !ok {
		return nil, nil
	}
	return &prev, nil
}

func (m *memStore) DeleteGiteaComment(_ context.Context, id int64, _ time.Time) error {
	delete(m.comments, id)
	return nil
}

func (m *memStore) Emit(_ context.Context, e notify.Event) error {
	m.events = append(m.events, e)
	return nil
}

var t0 = time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)

func base(st Status) Ticket {
	return Ticket{ID: 1, ProjectID: 1, ProjectKey: "demo", Repo: "demo/api", Type: "bug", Title: "登录失败",
		Status: st, UserID: "u1", IssueNumber: 12}
}

func newSync(t *testing.T, m *memStore, now time.Time) *Sync {
	t.Helper()
	s, err := NewSync(SyncOptions{Store: m, NeedInfoLabel: "lingo/review/need-info", Now: func() time.Time { return now },
		IssueURL: func(r string, n int64) string { return IssueURL("https://git.example.com/", r, n) }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func issue(c IssueChange, st IssueState) IssueUpdate {
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = t0
	}
	return IssueUpdate{Change: c, Repo: "Demo/API", Number: 12, DeliveryID: "d", ReceivedAt: t0, Current: st}
}

func statuses(es []notify.Event) string {
	var out []string
	for _, e := range es {
		out = append(out, string(e.Kind)+":"+e.PrevStatus+">"+e.Status)
	}
	return strings.Join(out, ",")
}

func TestApplyIssueTransitions(t *testing.T) {
	for _, c := range []struct {
		name   string
		from   Status
		fixed  int64
		u      IssueUpdate
		want   Status
		events string
	}{
		{"assigned", StatusOpen, 0, issue(IssueAssigned, IssueState{State: "open", Assignees: []string{"dev"}}), StatusInProgress, "ticket.status:open>in_progress"},
		{"assigned already in progress", StatusInProgress, 0, issue(IssueAssigned, IssueState{State: "open", Assignees: []string{"dev"}}), StatusInProgress, ""},
		{"unassigned snapshot", StatusOpen, 0, issue(IssueAssigned, IssueState{State: "open"}), StatusOpen, ""},
		{"need-info label", StatusInProgress, 0, issue(IssueLabeled, IssueState{State: "open", Labels: []string{"Lingo/Review/Need-Info"}}), StatusNeedInfo, "ticket.status:in_progress>need_info"},
		{"need-info removed", StatusNeedInfo, 0, issue(IssueLabeled, IssueState{State: "open", Labels: []string{"lingo/type/bug"}}), StatusInProgress, "ticket.status:need_info>in_progress"},
		{"closed with fix", StatusInProgress, 7, issue(IssueClosed, IssueState{State: "closed"}), StatusFixed, "ticket.status:in_progress>fixed"},
		{"closed already fixed", StatusFixed, 7, issue(IssueClosed, IssueState{State: "closed"}), StatusFixed, ""},
		{"closed but reopened since", StatusInProgress, 0, issue(IssueClosed, IssueState{State: "open"}), StatusInProgress, ""},
		{"reopened from fixed", StatusReleased, 7, issue(IssueReopened, IssueState{State: "open"}), StatusInProgress, "ticket.status:released>in_progress"},
		{"reopened but closed since", StatusClosed, 0, issue(IssueReopened, IssueState{State: "closed"}), StatusClosed, ""},
		{"deleted", StatusInProgress, 0, issue(IssueDeleted, IssueState{}), StatusAbnormal, "ticket.status:in_progress>abnormal"},
		{"deleted when closed", StatusClosed, 0, issue(IssueDeleted, IssueState{}), StatusClosed, ""},
		{"edited", StatusOpen, 0, issue(IssueEdited, IssueState{State: "open"}), StatusOpen, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			tk := base(c.from)
			tk.FixedPR = c.fixed
			m := newMem(tk)
			if err := newSync(t, m, t0.Add(time.Hour)).ApplyIssue(context.Background(), c.u); err != nil {
				t.Fatal(err)
			}
			got := m.tickets[1]
			if got.Status != c.want || statuses(m.events) != c.events {
				t.Errorf("status %s events %q; want %s %q", got.Status, statuses(m.events), c.want, c.events)
			}
			if len(m.history) != 1 {
				t.Errorf("history = %d", len(m.history))
			}
			if c.u.Change == IssueReopened && c.want == StatusInProgress && (got.FixedPR != 0 || got.FixedCommit != "") {
				t.Errorf("reopen kept fix: %+v", got)
			}
			if len(m.events) > 0 && m.events[0].URL != "https://git.example.com/demo/api/issues/12" {
				t.Errorf("url = %s", m.events[0].URL)
			}
		})
	}
}

func TestApplyIssueSynced(t *testing.T) {
	later := t0.Add(time.Hour)
	for _, c := range []struct {
		name   string
		from   Status
		fixed  int64
		seen   time.Time // 已采纳的 updated_at
		st     IssueState
		want   Status
		events int
	}{
		{"missed close with fix", StatusInProgress, 3, t0, IssueState{State: "closed", UpdatedAt: later}, StatusFixed, 1},
		{"missed close", StatusInProgress, 0, t0, IssueState{State: "closed", UpdatedAt: later}, StatusClosed, 1},
		{"missed reopen", StatusClosed, 0, t0, IssueState{State: "open", UpdatedAt: later}, StatusInProgress, 1},
		{"fixed but issue still open", StatusFixed, 3, t0, IssueState{State: "open", UpdatedAt: later}, StatusFixed, 0},
		{"missed assign", StatusOpen, 0, t0, IssueState{State: "open", Assignees: []string{"dev"}, UpdatedAt: later}, StatusInProgress, 1},
		{"missed need-info", StatusInProgress, 0, t0, IssueState{State: "open", Labels: []string{"lingo/review/need-info"}, UpdatedAt: later}, StatusNeedInfo, 1},
		{"already seen", StatusInProgress, 0, later, IssueState{State: "closed", UpdatedAt: later}, StatusInProgress, 0},
		{"missed milestone", StatusInProgress, 0, t0, IssueState{State: "open", Milestone: "v2", UpdatedAt: later}, StatusInProgress, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			tk := base(c.from)
			tk.FixedPR, tk.GiteaUpdatedAt = c.fixed, c.seen
			m := newMem(tk)
			s := newSync(t, m, t0.Add(2*time.Hour))
			if err := s.ApplyIssue(context.Background(), IssueUpdate{Change: IssueSynced, Repo: "demo/api", Number: 12, Current: c.st}); err != nil {
				t.Fatal(err)
			}
			if got := m.tickets[1]; got.Status != c.want || len(m.events) != c.events {
				t.Errorf("status %s events %d", got.Status, len(m.events))
			}
			if c.name == "already seen" && len(m.history) != 0 {
				t.Error("already-seen snapshot recorded history")
			}
		})
	}
}

func TestApplyPullReplaySkipsSeen(t *testing.T) {
	m := newMem(base(StatusInProgress))
	s := newSync(t, m, t0)
	ctx := context.Background()
	u := PullUpdate{Change: PullMerged, Repo: "demo/api", Number: 3, MergeCommit: "abc", Refs: []IssueRef{{Repo: "demo/api", Number: 12, Closes: true}}}
	if err := s.ApplyPull(ctx, u); err != nil {
		t.Fatal(err)
	}
	reopenTicket(m.tickets[1]) // 研发随后重开
	u.Replay = true
	if err := s.ApplyPull(ctx, u); err != nil {
		t.Fatal(err)
	}
	if m.tickets[1].Status != StatusInProgress || len(m.events) != 1 {
		t.Errorf("replayed merge undid the reopen: %s, %d events", m.tickets[1].Status, len(m.events))
	}
}

func TestApplyIssueClosedWithoutFixWaitsForPull(t *testing.T) {
	m := newMem(base(StatusInProgress))
	u := issue(IssueClosed, IssueState{State: "closed"})
	var de *DeferError
	if err := newSync(t, m, t0.Add(5*time.Second)).ApplyIssue(context.Background(), u); !errors.As(err, &de) || !de.Until.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("err = %v", err)
	}
	if m.tickets[1].Status != StatusInProgress || len(m.history) != 0 {
		t.Fatal("deferred event changed state")
	}
	// 等待期内 PR 合并事件先处理
	s := newSync(t, m, t0.Add(10*time.Second))
	if err := s.ApplyPull(context.Background(), PullUpdate{Change: PullMerged, Repo: "demo/api", Number: 3, MergeCommit: "abc",
		Refs: []IssueRef{{Repo: "demo/api", Number: 12, Closes: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := newSync(t, m, t0.Add(time.Minute)).ApplyIssue(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	got := m.tickets[1]
	if got.Status != StatusFixed || got.FixedPR != 3 || got.FixedCommit != "abc" || statuses(m.events) != "ticket.status:in_progress>fixed" {
		t.Errorf("ticket %+v events %s", got, statuses(m.events))
	}
	// 超过等待期仍无 PR：关闭未修复
	m2 := newMem(base(StatusOpen))
	if err := newSync(t, m2, t0.Add(time.Minute)).ApplyIssue(context.Background(), u); err != nil || m2.tickets[1].Status != StatusClosed {
		t.Errorf("closed: %v %s", err, m2.tickets[1].Status)
	}
}

func TestApplyIssueClosedWaitsForQueuedMerge(t *testing.T) {
	m := newMem(base(StatusInProgress))
	pending := true
	var asked time.Time
	s, _ := NewSync(SyncOptions{Store: m, Now: func() time.Time { return t0.Add(time.Hour) },
		PendingMerges: func(_ context.Context, repo string, before time.Time) (bool, error) {
			if repo != "Demo/API" {
				t.Errorf("repo = %s", repo)
			}
			asked = before
			return pending, nil
		}})
	u := issue(IssueClosed, IssueState{State: "closed"})
	var de *DeferError
	if err := s.ApplyIssue(context.Background(), u); !errors.As(err, &de) || !asked.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("err = %v, asked %v", err, asked)
	}
	pending = false
	if err := s.ApplyIssue(context.Background(), u); err != nil || m.tickets[1].Status != StatusClosed {
		t.Errorf("after queue drained: %v %s", err, m.tickets[1].Status)
	}
}

func TestApplyIssueDeletedIgnoresStaleness(t *testing.T) {
	tk := base(StatusInProgress)
	tk.GiteaUpdatedAt = t0.Add(time.Minute)
	m := newMem(tk)
	if err := newSync(t, m, t0.Add(time.Hour)).ApplyIssue(context.Background(), issue(IssueDeleted, IssueState{UpdatedAt: t0})); err != nil {
		t.Fatal(err)
	}
	if m.tickets[1].Status != StatusAbnormal {
		t.Errorf("status = %s", m.tickets[1].Status)
	}
}

func TestApplyIssueRecoversFromAbnormal(t *testing.T) {
	for _, c := range []struct {
		state string
		fixed int64
		want  Status
	}{{"open", 0, StatusInProgress}, {"closed", 0, StatusClosed}, {"closed", 3, StatusFixed}} {
		tk := base(StatusAbnormal)
		tk.FixedPR = c.fixed
		m := newMem(tk)
		if err := newSync(t, m, t0.Add(time.Hour)).ApplyIssue(context.Background(), issue(IssueLabeled, IssueState{State: c.state})); err != nil {
			t.Fatal(err)
		}
		if m.tickets[1].Status != c.want {
			t.Errorf("%s fixed=%d: %s, want %s", c.state, c.fixed, m.tickets[1].Status, c.want)
		}
	}
}

func TestApplyIssueStaleOnlyRecorded(t *testing.T) {
	tk := base(StatusInProgress)
	tk.GiteaUpdatedAt = t0.Add(time.Minute)
	m := newMem(tk)
	if err := newSync(t, m, t0.Add(time.Hour)).ApplyIssue(context.Background(), issue(IssueClosed, IssueState{State: "closed", UpdatedAt: t0})); err != nil {
		t.Fatal(err)
	}
	if m.tickets[1].Status != StatusInProgress || len(m.events) != 0 || len(m.history) != 1 || m.history[0].Payload["stale"] != true {
		t.Errorf("stale event applied: %+v %+v", m.tickets[1], m.history)
	}
}

func TestApplyIssueMilestone(t *testing.T) {
	m := newMem(base(StatusInProgress))
	due := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	s := newSync(t, m, t0.Add(time.Hour))
	u := issue(IssueMilestoned, IssueState{State: "open", Milestone: "v1.3", DueDate: &due})
	for range 2 { // 重放不重复通知
		if err := s.ApplyIssue(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.events) != 1 || m.events[0].Kind != notify.TicketSchedule || m.events[0].Body != "计划版本 v1.3，预计 2026-12-01" {
		t.Errorf("events = %+v", m.events)
	}
}

func TestApplyIssueLinksByMarker(t *testing.T) {
	tk := base(StatusSubmitted)
	tk.IssueNumber = 0
	other := base(StatusSubmitted)
	other.ID, other.IssueNumber, other.Repo = 2, 0, "demo/web"
	m := newMem(tk, other)
	s := newSync(t, m, t0.Add(time.Hour))
	u := issue(IssueOpened, IssueState{State: "open"})
	u.MarkerTicketID = 2 // 标记指向其他仓库的工单：不采信
	if err := s.ApplyIssue(context.Background(), u); err != nil || m.tickets[2].IssueNumber != 0 {
		t.Fatalf("linked mismatched repo: %v %+v", err, m.tickets[2])
	}
	u.MarkerTicketID = 1
	if err := s.ApplyIssue(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if got := m.tickets[1]; got.IssueNumber != 12 || got.Status != StatusOpen || statuses(m.events) != "ticket.status:submitted>open" {
		t.Errorf("ticket %+v events %s", got, statuses(m.events))
	}
}

func TestApplyCommentPublicOnce(t *testing.T) {
	m := newMem(base(StatusInProgress))
	s := newSync(t, m, t0)
	ctx := context.Background()
	steps := []CommentUpdate{
		{Change: CommentCreated, CommentID: 5, Body: "内部讨论"},
		{Change: CommentEdited, CommentID: 5, Body: "已修复", Public: true},
		{Change: CommentEdited, CommentID: 5, Body: "已修复，请重试", Public: true},
		{Change: CommentCreated, CommentID: 6, Body: "灵工转发", FromBot: true, Public: true},
		{Change: CommentDeleted, CommentID: 5},
	}
	for _, u := range steps {
		u.Repo, u.Number, u.Author = "demo/api", 12, "dev"
		if err := s.ApplyComment(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.events) != 1 || m.events[0].Kind != notify.TicketComment || m.events[0].Body != "已修复" {
		t.Errorf("events = %+v", m.events)
	}
	if len(m.comments) != 0 || len(m.history) != 4 {
		t.Errorf("comments %d history %d", len(m.comments), len(m.history))
	}
}

func TestApplyPull(t *testing.T) {
	a, b := base(StatusOpen), base(StatusInProgress)
	b.ID, b.IssueNumber = 2, 13
	m := newMem(a, b)
	s := newSync(t, m, t0)
	ctx := context.Background()
	refs := []IssueRef{{Repo: "demo/api", Number: 13, Closes: true}, {Repo: "demo/api", Number: 12}, {Repo: "demo/api", Number: 99, Closes: true}}
	if err := s.ApplyPull(ctx, PullUpdate{Change: PullOpened, Repo: "demo/api", Number: 3, Refs: refs}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPull(ctx, PullUpdate{Change: PullMerged, Repo: "demo/api", Number: 3, MergeCommit: "abc", Refs: refs}); err != nil {
		t.Fatal(err)
	}
	// #12 只是提及：合并不算修复；#13 带关闭关键词：已修复
	if m.tickets[1].Status != StatusInProgress || m.tickets[2].Status != StatusFixed || m.tickets[2].FixedCommit != "abc" {
		t.Errorf("tickets: %+v %+v", m.tickets[1], m.tickets[2])
	}
	if got := statuses(m.events); got != "ticket.status:open>in_progress,ticket.status:in_progress>fixed" {
		t.Errorf("events = %s", got)
	}
}
