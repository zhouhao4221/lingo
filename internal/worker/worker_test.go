package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
	"github.com/zhouhao4221/lingo/internal/webhook"
)

type outcome struct {
	how    string // complete | retry | abandon | fail
	reason string
	at     time.Time
}

type fakeQueue struct {
	deliveries    []store.Delivery
	notifications []store.Notification
	got           map[int64]outcome
}

func (q *fakeQueue) ClaimDeliveries(context.Context, int, time.Duration) ([]store.Delivery, error) {
	ds := q.deliveries
	q.deliveries = nil
	return ds, nil
}
func (q *fakeQueue) CompleteDelivery(_ context.Context, d store.Delivery) error {
	q.got[d.ID] = outcome{how: "complete"}
	return nil
}
func (q *fakeQueue) RetryDelivery(_ context.Context, d store.Delivery, r string, at time.Time) error {
	q.got[d.ID] = outcome{"retry", r, at}
	return nil
}
func (q *fakeQueue) ReleaseDelivery(_ context.Context, d store.Delivery, r string, at time.Time) error {
	q.got[d.ID] = outcome{"release", r, at}
	return nil
}
func (q *fakeQueue) AbandonDelivery(_ context.Context, d store.Delivery, r string) error {
	q.got[d.ID] = outcome{how: "abandon", reason: r}
	return nil
}
func (q *fakeQueue) ClaimNotifications(context.Context, int, time.Duration) ([]store.Notification, error) {
	ns := q.notifications
	q.notifications = nil
	return ns, nil
}
func (q *fakeQueue) CompleteNotification(_ context.Context, n store.Notification) error {
	q.got[n.OutboxID] = outcome{how: "complete"}
	return nil
}
func (q *fakeQueue) RetryNotification(_ context.Context, n store.Notification, r string, at time.Time) error {
	q.got[n.OutboxID] = outcome{"retry", r, at}
	return nil
}
func (q *fakeQueue) ReleaseNotification(_ context.Context, n store.Notification, r string, at time.Time) error {
	q.got[n.OutboxID] = outcome{"release", r, at}
	return nil
}
func (q *fakeQueue) FailNotification(_ context.Context, n store.Notification, r string) error {
	q.got[n.OutboxID] = outcome{how: "fail", reason: r}
	return nil
}

type fakeGitea struct{ err error }

func (f fakeGitea) GetIssue(context.Context, string, string, int64) (*gitea.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &gitea.Issue{Number: 12, State: "open"}, nil
}

type fakeTickets struct {
	issues   []ticket.IssueUpdate
	comments []ticket.CommentUpdate
	err      error
}

func (f *fakeTickets) Tracks(context.Context, string, int64) (bool, error) { return true, nil }
func (f *fakeTickets) ApplyIssue(_ context.Context, u ticket.IssueUpdate) error {
	f.issues = append(f.issues, u)
	return f.err
}
func (f *fakeTickets) ApplyComment(_ context.Context, u ticket.CommentUpdate) error {
	f.comments = append(f.comments, u)
	return f.err
}
func (f *fakeTickets) ApplyPull(context.Context, ticket.PullUpdate) error { return f.err }

var now = time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)

func newWorker(t *testing.T, q *fakeQueue, g IssueReader, tk Tickets, bot string, notifyFn func(context.Context, notify.Event) error, lease ...time.Duration) *Worker {
	t.Helper()
	o := Options{Deliveries: q, Notifications: q, Notify: notifyFn, Gitea: g, Tickets: tk,
		BotLogin: func() string { return bot }, Now: func() time.Time { return now }, MaxAttempts: 3,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if len(lease) > 0 {
		o.Lease = lease[0]
	}
	w, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func delivery(id int64, eventType string, kind webhook.Kind, body string, failures int) store.Delivery {
	return store.Delivery{ID: id, Attempts: failures + 1, Failures: failures, Record: webhook.Record{DeliveryID: "d", EventType: eventType, Kind: kind,
		Payload: []byte(body), ParseError: "bad json"}}
}

const commentBody = `{"action":"created","issue":{"id":1,"number":12},"comment":{"id":5,"body":"/reply 好了","user":{"login":"lingo-bot"}},"repository":{"full_name":"demo/api"},"sender":{"login":"lingo-bot"}}`
const labelBody = `{"action":"label_updated","issue":{"id":1,"number":12,"state":"closed"},"repository":{"full_name":"demo/api"},"sender":{"login":"dev"}}`

func TestProcessWebhooksOutcomes(t *testing.T) {
	q := &fakeQueue{got: map[int64]outcome{}}
	tk := &fakeTickets{}
	q.deliveries = []store.Delivery{
		delivery(1, "issues", webhook.Invalid, "{", 0),
		delivery(2, gitea.EventIssueComment, webhook.CommentCreated, commentBody, 0),
		delivery(3, gitea.EventIssueLabel, webhook.IssueLabeled, labelBody, 0),
	}
	// 机器人账号未知：评论推迟，标签事件照常处理并以 Gitea 当前状态为准
	if n, err := newWorker(t, q, fakeGitea{}, tk, "", nil).ProcessWebhooks(context.Background()); err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if o := q.got[1]; o.how != "abandon" {
		t.Errorf("invalid: %+v", o)
	}
	if o := q.got[2]; o.how != "release" || !o.at.Equal(now.Add(botRetry)) {
		t.Errorf("bot unknown: %+v", o)
	}
	if o := q.got[3]; o.how != "complete" || len(tk.issues) != 1 || tk.issues[0].Current.State != "open" {
		t.Errorf("label: %+v %+v", o, tk.issues)
	}

	// 机器人账号已知：机器人自己的评论标记为 FromBot
	q.deliveries = []store.Delivery{delivery(2, gitea.EventIssueComment, webhook.CommentCreated, commentBody, 0)}
	newWorker(t, q, fakeGitea{}, tk, "lingo-bot", nil).ProcessWebhooks(context.Background())
	if len(tk.comments) != 1 || !tk.comments[0].FromBot || tk.comments[0].Body != "好了" {
		t.Errorf("comments = %+v", tk.comments)
	}
}

func TestProcessWebhooksGiteaErrors(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		failures int
		want     string
		change   ticket.IssueChange
	}{
		{"not found means deleted", gitea.ErrNotFound, 0, "complete", ticket.IssueDeleted},
		{"server error retries", &gitea.APIError{StatusCode: 502}, 0, "retry", ""},
		{"client error falls back to payload", &gitea.APIError{StatusCode: 403}, 0, "complete", ticket.IssueLabeled},
		{"gives up after max failures", &gitea.APIError{StatusCode: 502}, 2, "abandon", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQueue{got: map[int64]outcome{}, deliveries: []store.Delivery{delivery(1, gitea.EventIssueLabel, webhook.IssueLabeled, labelBody, c.failures)}}
			tk := &fakeTickets{}
			newWorker(t, q, fakeGitea{err: c.err}, tk, "bot", nil).ProcessWebhooks(context.Background())
			if q.got[1].how != c.want {
				t.Errorf("outcome = %+v", q.got[1])
			}
			if c.change != "" && (len(tk.issues) != 1 || tk.issues[0].Change != c.change) {
				t.Errorf("issues = %+v", tk.issues)
			}
			if c.want == "retry" && !q.got[1].at.Equal(now.Add(10*time.Second)) {
				t.Errorf("retry at %v", q.got[1].at)
			}
		})
	}
}

func TestDeliverNotificationsFailuresAndPanic(t *testing.T) {
	q := &fakeQueue{got: map[int64]outcome{}}
	n := func(id int64, failures int) store.Notification {
		return store.Notification{OutboxID: id, Attempts: failures + 5, Failures: failures, Event: notify.Event{ID: id, TicketID: id}}
	}
	q.notifications = []store.Notification{n(1, 0), n(2, 2), n(3, 0), n(4, 0)}
	w := newWorker(t, q, fakeGitea{}, &fakeTickets{}, "bot", func(_ context.Context, e notify.Event) error {
		switch e.ID {
		case 3:
			panic("integrator bug")
		case 4:
			return nil
		}
		return errors.New("down")
	})
	if _, err := w.DeliverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 失败次数（而非领取次数）决定重试与放弃；Notifier 的 panic 按失败处理
	if o := q.got[1]; o.how != "retry" || !o.at.Equal(now.Add(10*time.Second)) {
		t.Errorf("1: %+v", o)
	}
	if o := q.got[2]; o.how != "fail" {
		t.Errorf("2: %+v", o)
	}
	if o := q.got[3]; o.how != "retry" || o.reason != "notifier panic: integrator bug" {
		t.Errorf("3: %+v", o)
	}
	if o := q.got[4]; o.how != "complete" {
		t.Errorf("4: %+v", o)
	}
}

func TestBatchReleasedWhenLeaseBudgetExhausted(t *testing.T) {
	q := &fakeQueue{got: map[int64]outcome{}}
	q.deliveries = []store.Delivery{delivery(1, gitea.EventIssueLabel, webhook.IssueLabeled, labelBody, 0),
		delivery(2, gitea.EventIssueLabel, webhook.IssueLabeled, labelBody, 0)}
	// 租约 20 秒：处理一条后剩余租约不足以再处理一条
	newWorker(t, q, fakeGitea{}, &fakeTickets{}, "bot", nil, 20*time.Second).ProcessWebhooks(context.Background())
	if q.got[1].how != "complete" || q.got[2].how != "release" {
		t.Errorf("outcomes = %+v", q.got)
	}
}

func TestBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{1: 10 * time.Second, 2: 20 * time.Second, 4: 80 * time.Second, 20: time.Hour} {
		if got := backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}
