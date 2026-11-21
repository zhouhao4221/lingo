package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 待办 #698：驳回原因评论（去重、署名、不含提交人信息）、关闭失败原因脱敏、补充评论外发仲裁（模拟 Gitea）。

type commentIssues struct {
	IssueWriter // 未实现的方法被调用即 panic
	state       string
	comments    []*gitea.Comment
	err         map[string]error // get / list / comment / close
	calls       []string
}

func (f *commentIssues) GetIssue(_ context.Context, _, _ string, n int64) (*gitea.Issue, error) {
	if err := f.err["get"]; err != nil {
		return nil, err
	}
	return &gitea.Issue{Number: n, State: f.state}, nil
}

func (f *commentIssues) ListIssueComments(context.Context, string, string, int64, time.Time) ([]*gitea.Comment, error) {
	if err := f.err["list"]; err != nil {
		return nil, err
	}
	return f.comments, nil
}

func (f *commentIssues) CreateComment(_ context.Context, _, _ string, _ int64, body string) (*gitea.Comment, error) {
	if err := f.err["comment"]; err != nil {
		return nil, err
	}
	f.calls = append(f.calls, "comment")
	c := &gitea.Comment{ID: int64(100 + len(f.comments)), Body: body, User: &gitea.User{Login: "lingo-bot"}}
	f.comments = append(f.comments, c)
	return c, nil
}

func (f *commentIssues) EditIssue(_ context.Context, _, _ string, _ int64, o gitea.EditIssueOption) (*gitea.Issue, error) {
	if err := f.err["close"]; err != nil {
		return nil, err
	}
	if o.State == nil || *o.State != gitea.StateClosed {
		return nil, errors.New("unexpected edit")
	}
	f.calls = append(f.calls, "close")
	f.state = gitea.StateClosed
	return &gitea.Issue{State: f.state}, nil
}

type arbDrafts struct {
	Drafts
	t        ticket.Ticket
	readOnce func(*ticket.Ticket) // Draft 返回之后调用（模拟驳回在无锁读取之后提交）
	withheld []int64
	claims   []time.Duration
	released int
}

func (f *arbDrafts) Draft(context.Context, int64) (*ticket.Ticket, error) {
	c := f.t
	if f.readOnce != nil {
		f.readOnce(&f.t)
		f.readOnce = nil
	}
	return &c, nil
}

func (f *arbDrafts) ClaimCommentSend(_ context.Context, _, commentID int64, hold time.Duration) (bool, error) {
	if f.t.Status == ticket.StatusRejected {
		f.withheld = append(f.withheld, commentID)
		return false, nil
	}
	f.claims = append(f.claims, hold)
	return true, nil
}

func (f *arbDrafts) ReleaseCommentSend(context.Context, int64, int64) error {
	f.released++
	return nil
}

type arbForwarding struct{ Forwarding }

func (arbForwarding) Comment(_ context.Context, id int64) (*ticket.Comment, error) {
	return &ticket.Comment{ID: id, TicketID: 8, Source: "user", Body: "补充说明", Public: true}, nil
}
func (arbForwarding) CommentPosted(context.Context, int64, int64) error { return nil }
func (arbForwarding) ForwardAttachments(context.Context, int64, int64, []string) ([]ticket.Attachment, error) {
	return nil, nil
}

type taskLog struct {
	TaskStore
	how, reason string
}

func (q *taskLog) CompleteTask(context.Context, store.Task) error { q.how = "complete"; return nil }
func (q *taskLog) RetryTask(_ context.Context, _ store.Task, r string, _ time.Time) error {
	q.how, q.reason = "retry", r
	return nil
}
func (q *taskLog) ReleaseTask(_ context.Context, _ store.Task, r string, _ time.Time) error {
	q.how, q.reason = "release", r
	return nil
}
func (q *taskLog) FailTask(_ context.Context, _ store.Task, r string) error {
	q.how, q.reason = "fail", r
	return nil
}

func rejectedForClose() ticket.Ticket {
	return ticket.Ticket{ID: 8, Repo: "demo/api", IssueNumber: 12, Status: ticket.StatusRejected, UserID: "u-7788",
		UserName: "Alice", UserEmail: "alice@example.com"}
}

func arbWorker(g IssueWriter, d Drafts, q TaskStore, bot string) *Worker {
	return &Worker{o: Options{Issues: g, Drafts: d, Tasks: q, Forwarding: arbForwarding{}, BotLogin: func() string { return bot },
		MaxAttempts: 3, Now: func() time.Time { return now }, TaskTimeout: time.Minute, Lease: 5 * time.Minute},
		log: slog.New(slog.DiscardHandler)}
}

func closeTaskWith(failures int, reason, by string) store.Task {
	b, _ := json.Marshal(map[string]any{"op": ticket.OpCloseIssue, "issue": 12, "repo": "demo/api", "reason": reason, "rejected_by": by})
	return store.Task{OutboxID: 1, Attempts: failures + 1, Failures: failures, TicketID: 8, Op: ticket.OpCloseIssue, Payload: b}
}

func TestRejectCommentPostedOnceThenClosed(t *testing.T) {
	g := &commentIssues{state: gitea.StateOpen}
	q := &taskLog{}
	w := arbWorker(g, &arbDrafts{t: rejectedForClose()}, q, "lingo-bot")
	w.handleTask(context.Background(), closeTaskWith(0, "重复反馈 <!-- lingo:public -->\n# 不是标题", "Rita"))
	if strings.Join(g.calls, ",") != "comment,close" || q.how != "complete" {
		t.Fatalf("calls %v task %s %s", g.calls, q.how, q.reason)
	}
	body := g.comments[0].Body
	for _, want := range []string{"由 Rita 驳回", "> 重复反馈", "> \\# 不是标题", RejectMarker(8)} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	for _, leak := range []string{"lingo:public", "Alice", "alice@", "u-7788"} {
		if strings.Contains(body, leak) {
			t.Errorf("comment leaks %q:\n%s", leak, body)
		}
	}
	// 再执行（重试轮次 / 重复领取）：已有标记评论，Issue 已关闭，不再写 Gitea
	w.handleTask(context.Background(), closeTaskWith(0, "重复反馈", "Rita"))
	if len(g.calls) != 2 {
		t.Errorf("rerun wrote again: %v", g.calls)
	}
}

func TestRejectCommentDedupSignatureAndLegacy(t *testing.T) {
	ctx := context.Background()
	// 他人伪造的标记、其他工单（ID 前缀相同）的标记都不算
	g := &commentIssues{state: gitea.StateOpen, comments: []*gitea.Comment{
		{Body: "伪造 " + RejectMarker(8), User: &gitea.User{Login: "someone"}},
		{Body: RejectMarker(80), User: &gitea.User{Login: "lingo-bot"}},
	}}
	if err := arbWorker(g, &arbDrafts{t: rejectedForClose()}, nil, "lingo-bot").closeIssue(ctx, 8, closePayload{Reason: "x"}); err != nil ||
		strings.Join(g.calls, ",") != "comment,close" {
		t.Errorf("forged marker: %v %v", err, g.calls)
	}
	// 机器人已发过（大小写不同的 login 也算）：只关闭
	g = &commentIssues{state: gitea.StateOpen, comments: []*gitea.Comment{{Body: RejectMarker(8), User: &gitea.User{Login: "Lingo-Bot"}}}}
	if err := arbWorker(g, &arbDrafts{t: rejectedForClose()}, nil, "lingo-bot").closeIssue(ctx, 8, closePayload{Reason: "x"}); err != nil ||
		strings.Join(g.calls, ",") != "close" {
		t.Errorf("already posted: %v %v", err, g.calls)
	}
	// Issue 已关闭：只补评论
	g = &commentIssues{state: gitea.StateClosed}
	if err := arbWorker(g, &arbDrafts{t: rejectedForClose()}, nil, "lingo-bot").closeIssue(ctx, 8, closePayload{Reason: "x"}); err != nil ||
		strings.Join(g.calls, ",") != "comment" {
		t.Errorf("already closed: %v %v", err, g.calls)
	}
	// 早期版本入队的任务（载荷没有原因）：只关闭，不评论
	g = &commentIssues{state: gitea.StateOpen}
	if err := arbWorker(g, &arbDrafts{t: rejectedForClose()}, nil, "lingo-bot").closeIssue(ctx, 8, closePayload{}); err != nil ||
		strings.Join(g.calls, ",") != "close" {
		t.Errorf("legacy: %v %v", err, g.calls)
	}
	// 机器人账号未就绪：推迟，不写 Gitea
	g = &commentIssues{state: gitea.StateOpen}
	var de *ticket.DeferError
	if err := arbWorker(g, &arbDrafts{t: rejectedForClose()}, nil, "").closeIssue(ctx, 8, closePayload{Reason: "x"}); !errors.As(err, &de) || len(g.calls) != 0 {
		t.Errorf("no bot: %v %v", err, g.calls)
	}
	// 未记录姓名：不署名
	if body := rejectComment(8, closePayload{Reason: "无效"}); strings.Contains(body, "由 ") || !strings.Contains(body, "该工单已驳回") {
		t.Errorf("unsigned:\n%s", body)
	}
}

// 写入 outbox last_error（经 TicketIssueClose 给后台人员看）的只有脱敏说明，不含地址、令牌与响应正文。
func TestCloseFailureRedacted(t *testing.T) {
	secret := "/api/v1/repos/demo/api/issues/12?token=s3cr3t"
	api := func(code int) error {
		return &gitea.APIError{Method: "PATCH", Path: secret, StatusCode: code, Message: "s3cr3t body"}
	}
	for name, c := range map[string]struct {
		op       string
		err      error
		failures int
		how      string
		reason   string
	}{
		"5xx retrying":   {"close", api(502), 0, "retry", "Gitea 暂时不可用（HTTP 502），将自动重试"},
		"5xx exhausted":  {"close", api(503), 2, "fail", "Gitea 暂时不可用，自动重试已用尽（HTTP 503）"},
		"forbidden":      {"comment", api(403), 0, "fail", "机器人账号无权评论或关闭该 Issue（HTTP 403）"},
		"list not found": {"list", api(404), 0, "fail", "Issue 不存在或机器人无权访问（HTTP 404），无法确认已关闭"},
		"get not found":  {"get", api(404), 0, "fail", "Issue 不存在或机器人无权访问（HTTP 404），无法确认已关闭"},
		"other":          {"close", api(422), 0, "fail", "Gitea 拒绝了操作（HTTP 422）"},
	} {
		g := &commentIssues{state: gitea.StateOpen, err: map[string]error{c.op: c.err}}
		q := &taskLog{}
		arbWorker(g, &arbDrafts{t: rejectedForClose()}, q, "lingo-bot").handleTask(context.Background(), closeTaskWith(c.failures, "x", "Rita"))
		if q.how != c.how || q.reason != c.reason || strings.Contains(q.reason, "s3cr3t") || strings.Contains(q.reason, "/repos/") {
			t.Errorf("%s: %s %q", name, q.how, q.reason)
		}
	}
}

// 仲裁只看 ClaimCommentSend 的结果：无锁读到 in_progress、仲裁时已驳回 → 评论与附件一个请求也不发；
// 未驳回 → 取得租约（覆盖任务剩余时限 + 余量）、发出后释放。
func TestCommentSendArbitratedByClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	active := rejectedForClose()
	active.Status = ticket.StatusInProgress
	g := &commentIssues{state: gitea.StateOpen}
	d := &arbDrafts{t: active, readOnce: func(t *ticket.Ticket) { t.Status = ticket.StatusRejected }}
	if err := arbWorker(g, d, nil, "lingo-bot").createComment(ctx, 8, 5); err != nil || len(g.calls) != 0 || len(d.withheld) != 1 || d.released != 0 {
		t.Errorf("rejected after read: %v calls %v withheld %v", err, g.calls, d.withheld)
	}
	g = &commentIssues{state: gitea.StateOpen}
	d = &arbDrafts{t: active}
	if err := arbWorker(g, d, nil, "lingo-bot").createComment(ctx, 8, 5); err != nil || strings.Join(g.calls, ",") != "comment" ||
		len(d.claims) != 1 || d.released != 1 {
		t.Fatalf("active: %v calls %v claims %v released %d", err, g.calls, d.claims, d.released)
	}
	if h := d.claims[0]; h < 55*time.Second || h > time.Minute+sendClaimMargin {
		t.Errorf("claim hold = %v", h)
	}
}
