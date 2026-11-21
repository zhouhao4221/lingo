package tests

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// 待办 #698：补充评论外发与转研发后驳回的仲裁（PostgreSQL + 模拟 Gitea，顺序由用例控制，确定性）。

// 发送先取得仲裁：补充评论的 POST 在途（模拟 Gitea 挂起）期间驳回不得提交——等待 5 秒后返回 409、工单不变；
// 再次驳回在请求结束前不返回、不提交，请求结束后提交，随后关单链路照常（原因评论 + 关闭）。在途评论已发出，不追撤。
func TestRejectForwardedWaitsForInFlightComment(t *testing.T) {
	g := &giteaServer{templates: bugTemplate}
	l, _, count, id := newForwarded(t, g)
	ctx := context.Background()
	entered, release := g.holdNextComment(t)
	if _, err := l.AddTicketComment(ctx, alice, id, "在途的补充", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("comment POST not sent")
	}
	unchanged := func() bool {
		return count("SELECT count(*) FROM lingo_tickets WHERE id = "+jsonNum(id)+" AND status = 'open'") == 1 &&
			count("SELECT count(*) FROM lingo_outbox WHERE key LIKE 'close_issue:%'") == 0 &&
			count("SELECT count(*) FROM lingo_ticket_events WHERE kind = 'ticket.rejected'") == 0
	}

	// 1. 请求在途：驳回等待约 5 秒后冲突，没有提交任何改动
	start := time.Now()
	if _, err := l.RejectForwardedTicket(ctx, rita, id, "重复"); !errors.Is(err, lingo.ErrConflict) || time.Since(start) < 5*time.Second {
		t.Fatalf("reject while in flight: %v after %v", err, time.Since(start))
	}
	if !unchanged() {
		t.Fatal("reject committed while comment in flight")
	}

	// 2. 再次驳回：请求仍在途时不返回、不提交；放行后提交
	done := make(chan error, 1)
	go func() {
		_, err := l.RejectForwardedTicket(ctx, rita, id, "重复")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("reject returned while comment in flight: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if !unchanged() {
		t.Fatal("reject committed while comment in flight")
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reject after send: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reject did not finish after send")
	}
	waitIssueClose(t, l, id, "done")
	// 在途评论已发出并记录（不追撤，不算被拦下）；之后才是驳回原因评论
	if n := count("SELECT count(*) FROM lingo_ticket_comments WHERE gitea_posted_id IS NOT NULL AND sending_until IS NULL AND withheld_at IS NULL"); n != 1 {
		t.Errorf("posted comments = %d", n)
	}
	if n := count("SELECT count(*) FROM lingo_ticket_events WHERE kind = 'comment.withheld'"); n != 0 {
		t.Errorf("withheld audit = %d", n)
	}
	if closes, comments, _ := g.snapshot(); len(closes) != 1 || len(comments) != 2 || !strings.Contains(comments[0], "在途的补充") ||
		!strings.Contains(comments[1], "lingo:reject=") {
		t.Errorf("gitea closes %v comments %q", closes, comments)
	}
}

// 驳回先提交：worker 停止期间排队的补充评论在驳回后执行时一个请求也不发（评论、附件均不发），评论留在灵工，
// 记一条 comment.withheld；关单链路照常。
func TestRejectForwardedBeforeQueuedCommentSends(t *testing.T) {
	g := &giteaServer{templates: bugTemplate}
	l, db, _ := newTicketLingo(t, g, nil, map[lingo.TicketType]bool{lingo.TicketBug: true})
	count := func(q string) (n int) {
		t.Helper()
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ctx := context.Background()
	stop := startLingo(l)
	tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "登录失败", Fields: map[string][]string{"what": {"登录失败"}}})
	if err != nil {
		t.Fatal(err)
	}
	waitTicket(t, l, rita, tk.ID, "open")
	stop() // worker 停止：之后的补充只入队、不执行

	c, err := l.AddTicketComment(ctx, alice, tk.ID, "驳回前排队的补充", []lingo.Attachment{{ID: "f1", Name: "a.png", URL: "https://app.example.com/f1"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := count("SELECT count(*) FROM lingo_outbox WHERE key LIKE 'create_comment:%' AND status = 'pending'"); n != 1 {
		t.Fatalf("queued comment tasks = %d", n)
	}
	if _, err := l.RejectForwardedTicket(ctx, rita, tk.ID, "重复"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(startLingo(l))
	waitIssueClose(t, l, tk.ID, "done")
	waitUntil(t, "queued comment task done", func() bool {
		return count("SELECT count(*) FROM lingo_outbox WHERE key LIKE 'create_comment:%' AND status = 'done'") == 1
	})
	if n := count("SELECT count(*) FROM lingo_ticket_comments WHERE id = " + jsonNum(c.ID) + " AND gitea_posted_id IS NULL AND withheld_at IS NOT NULL AND forwarded_at IS NULL"); n != 1 {
		t.Errorf("withheld comment rows = %d", n)
	}
	if n := count("SELECT count(*) FROM lingo_ticket_events WHERE kind = 'comment.withheld' AND source = 'system' AND (payload->>'comment')::bigint = " + jsonNum(c.ID)); n != 1 {
		t.Errorf("withheld audit = %d", n)
	}
	g.mu.Lock()
	assets := len(g.assets)
	g.mu.Unlock()
	if _, comments, _ := g.snapshot(); len(comments) != 1 || !strings.Contains(comments[0], "lingo:reject=") || assets != 0 {
		t.Errorf("gitea comments %q assets %d", comments, assets)
	}
	cs, err := l.ListTicketComments(ctx, alice, tk.ID)
	if err != nil || len(cs) != 1 || cs[0].Body != "驳回前排队的补充" {
		t.Errorf("lingo comments = %+v %v", cs, err)
	}
}

// startLingo 运行 l.Run，返回停止并等待退出的函数（可重复调用）。
func startLingo(l *lingo.Lingo) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = l.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}
