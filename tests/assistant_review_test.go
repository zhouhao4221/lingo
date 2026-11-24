package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
	"github.com/zhouhao4221/lingo/internal/assistant"
	"github.com/zhouhao4221/lingo/internal/store"
)

// PR #33 审查问题的回归用例。

// 确认判断期间操作被取消：执行时发现不在等待中，如实回复并释放租约，会话可以继续提问。
func TestAssistantConfirmAfterConcurrentCancel(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	id := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)
	conv, _ := l.CreateConversation(ctx, alice, "")
	m.intents = []string{fmt.Sprintf(`{"intent":"comment","ticket_ids":[%d],"comment_body":"复现环境是 Chrome 120"}`, id)}
	p := send(t, l, alice, conv.ID, fmt.Sprintf("在 #%d 留言：复现环境是 Chrome 120", id)).PendingAction
	m.decisions = []string{"confirm"}
	m.onDecision = func() { // 判断期间操作被作废（如另一处点了取消）
		if _, err := db.Exec(`UPDATE lingo_assistant_actions SET status = 'cancelled', resolved_at = now() WHERE id = $1`, p.ID); err != nil {
			t.Error(err)
		}
	}
	got := send(t, l, alice, conv.ID, "确认")
	if !strings.Contains(got.Content, "已经取消") {
		t.Errorf("content = %q", got.Content)
	}
	m.intents = []string{`{"intent":"out_of_scope"}`}
	send(t, l, alice, conv.ID, "还能继续问吗") // 租约已释放
	if cs, _ := l.ListTicketComments(ctx, alice, id); len(cs) != 0 {
		t.Errorf("comments = %d", len(cs))
	}
}

// 按钮与一轮提问互斥：回答进行中点确认 / 取消返回冲突。
func TestAssistantButtonsWaitForTurn(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	id := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)
	conv, _ := l.CreateConversation(ctx, alice, "")
	m.intents = []string{fmt.Sprintf(`{"intent":"comment","ticket_ids":[%d],"comment_body":"复现环境是 Chrome 120"}`, id)}
	p := send(t, l, alice, conv.ID, fmt.Sprintf("在 #%d 留言：复现环境是 Chrome 120", id)).PendingAction

	gate, entered := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	m.gate, m.entered = gate, entered
	m.decisions = []string{"other"}
	m.intents = []string{`{"intent":"out_of_scope"}`}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); send(t, l, alice, conv.ID, "这个功能怎么用") }()
	<-entered
	if _, err := l.ConfirmPendingAction(ctx, alice, conv.ID, p.ID); !errors.Is(err, lingo.ErrConflict) {
		t.Errorf("confirm during turn: %v", err)
	}
	if _, err := l.CancelPendingAction(ctx, alice, conv.ID, p.ID); !errors.Is(err, lingo.ErrConflict) {
		t.Errorf("cancel during turn: %v", err)
	}
	close(gate)
	<-done
}

// 失败回答之后插入了按钮的结果消息，仍能重试。
func TestAssistantRetryAfterButtonMessage(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	id := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)
	conv, _ := l.CreateConversation(ctx, alice, "")
	m.intents = []string{fmt.Sprintf(`{"intent":"comment","ticket_ids":[%d],"comment_body":"复现环境是 Chrome 120"}`, id)}
	p := send(t, l, alice, conv.ID, fmt.Sprintf("在 #%d 留言：复现环境是 Chrome 120", id)).PendingAction
	m.fail = 1 // 下一条提问的确认判断失败：回答 failed，操作保持等待
	failed, err := l.SendMessage(ctx, alice, conv.ID, lingo.MessageInput{Content: "最近发版了什么"})
	if err != nil || failed.Status != "failed" {
		t.Fatalf("failed = %+v %v", failed, err)
	}
	if r, err := l.ConfirmPendingAction(ctx, alice, conv.ID, p.ID); err != nil || r.Message == nil {
		t.Fatalf("button confirm: %+v %v", r, err)
	}
	m.intents = []string{`{"intent":"release_list"}`}
	if got, err := l.RetryMessage(ctx, alice, conv.ID, failed.ID, ""); err != nil || got.Status != "done" {
		t.Errorf("retry after button message: %+v %v", got, err)
	}
}

// 确认判断输出不合格、意图解析认出了确认：照常执行，不作废操作。
func TestAssistantConfirmFallsBackToIntent(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	id := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)
	conv, _ := l.CreateConversation(ctx, alice, "")
	m.intents = []string{fmt.Sprintf(`{"intent":"comment","ticket_ids":[%d],"comment_body":"复现环境是 Chrome 120"}`, id)}
	send(t, l, alice, conv.ID, fmt.Sprintf("在 #%d 留言：复现环境是 Chrome 120", id))
	m.decisions = []string{"???"} // 不合格：按 other 处理
	m.intents = []string{`{"intent":"confirm"}`}
	got := send(t, l, alice, conv.ID, "确认")
	cs, _ := l.ListTicketComments(ctx, alice, id)
	if !strings.Contains(got.Content, "已在") || len(cs) != 1 {
		t.Errorf("content = %q, comments = %d", got.Content, len(cs))
	}
}

// 第二轮审查：落库失败后兜底记下失败回答（不沿用已回滚的消息 ID），并释放租约。
func TestAssistantSalvageAfterFinishFailure(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	id := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)
	conv, _ := l.CreateConversation(ctx, alice, "")
	// 让提出操作时的写入失败：finish 事务在 InsertMessage 之后回滚
	if _, err := db.Exec(`ALTER TABLE lingo_assistant_actions ADD CONSTRAINT no_insert CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	m.intents = []string{fmt.Sprintf(`{"intent":"comment","ticket_ids":[%d],"comment_body":"复现环境是 Chrome 120"}`, id)}
	got, err := l.SendMessage(ctx, alice, conv.ID, lingo.MessageInput{Content: fmt.Sprintf("在 #%d 留言：复现环境是 Chrome 120", id)})
	if err != nil || got.Status != "failed" || got.Error != "internal_error" || got.ID == 0 {
		t.Fatalf("salvaged = %+v %v", got, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM lingo_assistant_messages WHERE id = $1 AND status = 'failed'`, got.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("failed message stored = %d %v", n, err)
	}
	if _, err := db.Exec(`ALTER TABLE lingo_assistant_actions DROP CONSTRAINT no_insert`); err != nil {
		t.Fatal(err)
	}
	m.intents = []string{`{"intent":"out_of_scope"}`}
	if r, err := l.RetryMessage(ctx, alice, conv.ID, got.ID, ""); err != nil || r.Status != "done" { // 租约已释放，可重试
		t.Errorf("retry = %+v %v", r, err)
	}
}

// 第二轮审查：提问与历史中写到的工单号不算编造。
func TestAssistantGroundedAllowsAskedIDs(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	mine := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "released", 12)
	theirs := seedTicketRow(t, db, "u2", "", "api", "bug", "登录超时", "released", 13)
	want := fmt.Sprintf("#%d 已发布；没有找到 #%d。", mine, theirs)
	m.answer = func(string) string { return want }
	conv, _ := l.CreateConversation(ctx, alice, "")
	m.intents = []string{fmt.Sprintf(`{"intent":"ticket_find","ticket_ids":[%d,%d]}`, mine, theirs)}
	if got := send(t, l, alice, conv.ID, fmt.Sprintf("#%d 和 #%d 修了吗", mine, theirs)); got.Content != want {
		t.Errorf("content = %q", got.Content)
	}
}

// 第二轮审查：回答期间过期的操作记为 expired，而不是 cancelled。
func TestAssistantActionExpiresDuringTurn(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	id := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)
	conv, _ := l.CreateConversation(ctx, alice, "")
	m.intents = []string{fmt.Sprintf(`{"intent":"comment","ticket_ids":[%d],"comment_body":"复现环境是 Chrome 120"}`, id)}
	p := send(t, l, alice, conv.ID, fmt.Sprintf("在 #%d 留言：复现环境是 Chrome 120", id)).PendingAction
	m.decisions = []string{"other"}
	m.onDecision = func() {
		if _, err := db.Exec(`UPDATE lingo_assistant_actions SET expires_at = now() - interval '1 second' WHERE id = $1`, p.ID); err != nil {
			t.Error(err)
		}
	}
	m.intents = []string{`{"intent":"out_of_scope"}`}
	send(t, l, alice, conv.ID, "这个功能怎么用")
	var st string
	if err := db.QueryRow(`SELECT status FROM lingo_assistant_actions WHERE id = $1`, p.ID).Scan(&st); err != nil || st != "expired" {
		t.Errorf("status = %q %v", st, err)
	}
}

// 第二轮审查：租约只由持有者释放，超时后被别人取得的租约不受影响。
func TestAssistantLeaseOwnership(t *testing.T) {
	db := testDB(t, true)
	s, err := store.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := s.CreateConversation(ctx, "u1", "")
	if err != nil {
		t.Fatal(err)
	}
	claim := func() (time.Time, bool) {
		var lease time.Time
		var ok bool
		if err := s.InAssistantTx(ctx, func(tx assistant.Tx) error {
			cv, got, err := tx.ClaimTurn(ctx, "u1", c.ID, time.Minute)
			if got {
				lease = cv.Lease
			}
			ok = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return lease, ok
	}
	first, ok := claim()
	if !ok {
		t.Fatal("first claim")
	}
	if _, ok := claim(); ok {
		t.Fatal("claimed while held")
	}
	if _, err := db.Exec(`UPDATE lingo_assistant_conversations SET turn_until = now() - interval '1 second' WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := claim(); !ok { // 第一个租约已过期，被第二轮取得
		t.Fatal("second claim")
	}
	if err := s.InAssistantTx(ctx, func(tx assistant.Tx) error { return tx.ReleaseTurn(ctx, c.ID, first) }); err != nil {
		t.Fatal(err)
	}
	if _, ok := claim(); ok {
		t.Error("stale release cleared the new lease")
	}
}

// 第二轮审查：MCP ask 在未配置模型时不留下空会话。
func TestAssistantMCPAskNoModelNoConversation(t *testing.T) {
	l, _ := newAssistantLingo(t, nil)
	if res, ok := (mcpClient{t: t, h: l.MCPHandler()}).call("u1", "external", "ask", map[string]any{"question": "hi"}); ok {
		t.Fatalf("ask without model: %v", res)
	}
	if res, ok := (mcpClient{t: t, h: l.MCPHandler()}).call("u1", "external", "ask", map[string]any{"question": "   "}); ok {
		t.Fatalf("blank question: %v", res)
	}
	if cs, _ := l.ListConversations(context.Background(), alice, 0, 0); len(cs) != 0 {
		t.Errorf("conversations = %d", len(cs))
	}
}

// 版本说明里的 Gitea 编号改写为 "Issue N" 交给模型，模型照抄时回答不会被当成编造。
func TestAssistantReleaseNotesIssueRefs(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	a := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "released", 12)
	rel := seedReleaseRow(t, db, "v1.3.0", a)
	if _, err := db.Exec(`UPDATE lingo_releases SET body = '修复登录 (#45)' WHERE id = $1`, rel); err != nil {
		t.Fatal(err)
	}
	var facts string
	m.answer = func(data string) string { facts = data; return "v1.3.0 修复登录（Issue 45）" }
	conv, _ := l.CreateConversation(ctx, alice, "")
	m.intents = []string{`{"intent":"release_list"}`}
	got := send(t, l, alice, conv.ID, "最近发版了什么")
	if strings.Contains(facts, "#45") || !strings.Contains(facts, "Issue 45") {
		t.Errorf("facts = %s", facts)
	}
	if got.Content != "v1.3.0 修复登录（Issue 45）" {
		t.Errorf("content replaced: %q", got.Content)
	}
}
