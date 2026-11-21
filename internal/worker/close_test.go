package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

type fakeCloseIssues struct {
	IssueWriter
	state   string
	getErr  error
	editErr error
	closed  []int64
}

func (f *fakeCloseIssues) GetIssue(_ context.Context, _, _ string, n int64) (*gitea.Issue, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &gitea.Issue{Number: n, State: f.state}, nil
}

func (f *fakeCloseIssues) EditIssue(_ context.Context, _, _ string, n int64, o gitea.EditIssueOption) (*gitea.Issue, error) {
	if f.editErr != nil {
		return nil, f.editErr
	}
	if o.State == nil || *o.State != gitea.StateClosed || o.Body != nil || o.Title != nil || o.Assignees != nil {
		return nil, errors.New("unexpected edit")
	}
	f.closed = append(f.closed, n)
	f.state = gitea.StateClosed
	return &gitea.Issue{Number: n, State: f.state}, nil
}

type fakeCloseDrafts struct {
	Drafts
	t      *ticket.Ticket
	failed bool
}

func (f *fakeCloseDrafts) IssueFailed(context.Context, int64, string) error {
	f.failed = true
	return nil
}

type fakeTaskQueue struct{ got map[int64]outcome }

func (q *fakeTaskQueue) ClaimTasks(context.Context, int, time.Duration) ([]store.Task, error) {
	return nil, nil
}
func (q *fakeTaskQueue) CompleteTask(_ context.Context, t store.Task) error {
	q.got[t.OutboxID] = outcome{how: "complete"}
	return nil
}
func (q *fakeTaskQueue) RetryTask(_ context.Context, t store.Task, r string, at time.Time) error {
	q.got[t.OutboxID] = outcome{"retry", r, at}
	return nil
}
func (q *fakeTaskQueue) ReleaseTask(_ context.Context, t store.Task, r string, at time.Time) error {
	q.got[t.OutboxID] = outcome{"release", r, at}
	return nil
}
func (q *fakeTaskQueue) FailTask(_ context.Context, t store.Task, r string) error {
	q.got[t.OutboxID] = outcome{how: "fail", reason: r}
	return nil
}

func storeTask(id int64, op string, ticketID int64, failures int) store.Task {
	return store.Task{OutboxID: id, Attempts: failures + 1, Failures: failures, TicketID: ticketID, Op: op,
		Payload: []byte(`{"op":"` + op + `"}`)}
}

func (f *fakeCloseDrafts) Draft(context.Context, int64) (*ticket.Ticket, error) {
	if f.t == nil {
		return nil, ticket.ErrNotFound
	}
	c := *f.t
	return &c, nil
}

func TestCloseIssue(t *testing.T) {
	ctx := context.Background()
	rejected := &ticket.Ticket{ID: 8, Repo: "demo/api", IssueNumber: 12, Status: ticket.StatusRejected}
	newW := func(g *fakeCloseIssues, tk *ticket.Ticket) *Worker {
		return &Worker{o: Options{Issues: g, Drafts: &fakeCloseDrafts{t: tk}, Now: time.Now}, log: slog.New(slog.DiscardHandler)}
	}
	// 关闭库中记录的主 Issue；重试时已关闭则不再调用
	g := &fakeCloseIssues{state: gitea.StateOpen}
	w := newW(g, rejected)
	if err := w.closeIssue(ctx, 8, closePayload{}); err != nil || len(g.closed) != 1 || g.closed[0] != 12 {
		t.Fatalf("close: %v %v", err, g.closed)
	}
	if err := w.closeIssue(ctx, 8, closePayload{}); err != nil || len(g.closed) != 1 {
		t.Errorf("retry after close: %v %v", err, g.closed)
	}
	// 工单不是驳回状态、没有 Issue、或已不存在：不动 Gitea
	for _, tk := range []*ticket.Ticket{
		{ID: 8, Repo: "demo/api", IssueNumber: 12, Status: ticket.StatusInProgress},
		{ID: 8, Repo: "demo/api", Status: ticket.StatusRejected},
		nil,
	} {
		g := &fakeCloseIssues{state: gitea.StateOpen}
		if err := newW(g, tk).closeIssue(ctx, 8, closePayload{}); err != nil || len(g.closed) != 0 {
			t.Errorf("%+v: %v %v", tk, err, g.closed)
		}
	}
	// Issue 404（已删除、权限被收回、Issues 被禁用或单独拒权都是 404）：无法确认，不可重试的失败，原因可查
	nf := &gitea.APIError{StatusCode: 404}
	var pe *permanentError
	for _, g := range []*fakeCloseIssues{{getErr: nf}, {state: gitea.StateOpen, editErr: nf}} {
		err := newW(g, rejected).closeIssue(ctx, 8, closePayload{})
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), "not found") || len(g.closed) != 0 {
			t.Errorf("404: %v", err)
		}
	}
	// 5xx / 429 可重试；403 等不可重试，直接放弃并可查
	for _, c := range []struct {
		g         *fakeCloseIssues
		permanent bool
	}{
		{&fakeCloseIssues{getErr: &gitea.APIError{StatusCode: 502}}, false},
		{&fakeCloseIssues{state: gitea.StateOpen, editErr: &gitea.APIError{StatusCode: 429}}, false},
		{&fakeCloseIssues{state: gitea.StateOpen, editErr: &gitea.APIError{StatusCode: 403, Message: "forbidden"}}, true},
	} {
		err := newW(c.g, rejected).closeIssue(ctx, 8, closePayload{})
		if err == nil || errors.As(err, &pe) != c.permanent {
			t.Errorf("%+v: err %v, want permanent=%v", c.g, err, c.permanent)
		}
	}
}

type rejectedPerspDrafts struct {
	fakeCloseDrafts
	withheld []int64
}

// ClaimCommentSend 模拟发送仲裁：工单已驳回时不给租约（记入 withheld）。
func (f *rejectedPerspDrafts) ClaimCommentSend(_ context.Context, _, commentID int64, _ time.Duration) (bool, error) {
	if f.t.Status == ticket.StatusRejected {
		f.withheld = append(f.withheld, commentID)
		return false, nil
	}
	return true, nil
}

func (f *rejectedPerspDrafts) ReleaseCommentSend(context.Context, int64, int64) error { return nil }

type oneComment struct{ Forwarding }

func (oneComment) Comment(_ context.Context, id int64) (*ticket.Comment, error) {
	return &ticket.Comment{ID: id, TicketID: 8, Source: "user", Body: "驳回前排队的补充", Public: true}, nil
}

func (f *rejectedPerspDrafts) Perspectives(context.Context, int64) (*ticket.PerspectiveSet, error) {
	c := *f.t
	return &ticket.PerspectiveSet{Ticket: &c}, nil
}

// 驳回前已排队的评论、标签与视角同步在驳回后直接完成，不再写 Gitea（fakeCloseIssues 未实现这些方法，调用即 panic）。
func TestQueuedWritesSkippedAfterReject(t *testing.T) {
	ctx := context.Background()
	rejected := &ticket.Ticket{ID: 8, Type: ticket.TypeFeature, Repo: "demo/api", IssueNumber: 12, Status: ticket.StatusRejected}
	g := &fakeCloseIssues{state: gitea.StateOpen}
	d := &rejectedPerspDrafts{fakeCloseDrafts: fakeCloseDrafts{t: rejected}}
	w := &Worker{o: Options{Issues: g, Drafts: d, Forwarding: oneComment{}, Now: time.Now, TaskTimeout: time.Minute},
		log: slog.New(slog.DiscardHandler)}
	if err := w.createComment(ctx, 8, 5); err != nil || len(d.withheld) != 1 || d.withheld[0] != 5 {
		t.Errorf("comment: %v withheld %v", err, d.withheld)
	}
	if err := w.removeLabel(ctx, 8, "lingo/review/need-info"); err != nil {
		t.Errorf("label: %v", err)
	}
	if err := w.syncPerspectives(ctx, 8, 3); err != nil {
		t.Errorf("perspectives: %v", err)
	}
	if len(g.closed) != 0 {
		t.Errorf("gitea touched: %v", g.closed)
	}
}

// close_issue 任务经 handleTask：成功完成、可重试错误退避重试、达到上限或不可重试时放弃（failed，原因可查），
// 且不会像建单失败那样把工单改为 abnormal。
func TestCloseIssueTaskOutcomes(t *testing.T) {
	rejected := &ticket.Ticket{ID: 8, Repo: "demo/api", IssueNumber: 12, Status: ticket.StatusRejected}
	for _, c := range []struct {
		name     string
		g        *fakeCloseIssues
		failures int
		want     string
	}{
		{"ok", &fakeCloseIssues{state: gitea.StateOpen}, 0, "complete"},
		{"5xx", &fakeCloseIssues{getErr: &gitea.APIError{StatusCode: 503}}, 0, "retry"},
		{"5xx at limit", &fakeCloseIssues{getErr: &gitea.APIError{StatusCode: 503}}, 2, "fail"},
		{"403", &fakeCloseIssues{state: gitea.StateOpen, editErr: &gitea.APIError{StatusCode: 403}}, 0, "fail"},
		{"404", &fakeCloseIssues{getErr: &gitea.APIError{StatusCode: 404}}, 0, "fail"},
	} {
		q := &fakeTaskQueue{got: map[int64]outcome{}}
		drafts := &fakeCloseDrafts{t: rejected}
		w := &Worker{o: Options{Issues: c.g, Drafts: drafts, Tasks: q, Now: time.Now, MaxAttempts: 3}, log: slog.New(slog.DiscardHandler)}
		w.handleTask(context.Background(), storeTask(1, ticket.OpCloseIssue, 8, c.failures))
		if o := q.got[1]; o.how != c.want {
			t.Errorf("%s: %+v, want %s", c.name, o, c.want)
		}
		if drafts.failed {
			t.Errorf("%s: ticket marked abnormal", c.name)
		}
	}
}
