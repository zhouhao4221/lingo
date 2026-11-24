package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

const extractJSON = `{"skip":false,"question":"导出报表为空怎么办","answer":"检查筛选条件；已在 v1.2 修复"}`

// REQ-007：结案工单抽取（待审核、每张一次、可能重复、closed 带未修复语境），审核通过 / 驳回。
func TestKnowledgeExtractAndReview(t *testing.T) {
	m := &scriptModel{answer: func(data string) string {
		if strings.Contains(data, `"closed"`) {
			return extractJSON
		}
		return "好的"
	}}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	existing, err := l.CreateKnowledge(ctx, rita, lingo.KnowledgeDraft{Project: "demo", Question: "导出报表为空怎么办", Answer: "检查筛选条件"})
	if err != nil {
		t.Fatal(err)
	}
	rel := seedTicketRow(t, db, "u1", "", "api", "bug", "导出报表为空", "released", 21)
	_ = seedTicketRow(t, db, "u1", "", "api", "feature", "支持导出 PDF", "closed", 22)
	open := seedTicketRow(t, db, "u1", "", "api", "bug", "页面卡顿", "in_progress", 23)
	consult := seedTicketRow(t, db, "u1", "", "api", "consult", "怎么导出", "answered", 0)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- l.Run(runCtx) }()
	deadline := time.Now().Add(15 * time.Second)
	for countRows(t, db, "SELECT count(*) FROM lingo_knowledge_extractions WHERE status = 'done'") < 2 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("extraction not done")
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge_extractions WHERE ticket_id = ANY($1)", fmt.Sprintf("{%d,%d}", open, consult)); n != 0 {
		t.Errorf("non-closed / consult tickets enqueued: %d", n)
	}
	if !strings.Contains(m.allPrompts(), `"closed": true`) {
		t.Errorf("closed context not passed to extractor")
	}
	pend, err := l.ListKnowledge(ctx, rita, lingo.KnowledgeFilter{Status: lingo.KnowledgePending})
	if err != nil || len(pend) != 2 {
		t.Fatalf("pending = %+v %v", pend, err)
	}
	var fromRel lingo.KnowledgeEntry
	for _, e := range pend {
		if e.Source != lingo.KnowledgeSourceTicketExtract || e.DuplicateOf != existing.ID {
			t.Errorf("pending entry = %+v", e)
		}
		if e.SourceTicketID == rel {
			fromRel = e
		}
	}
	// 待审核不参与检索
	if got, _ := l.SearchKnowledge(ctx, alice, "导出报表为空", "", 0); len(got) != 1 || got[0].ID != existing.ID {
		t.Errorf("pending searchable: %v", knowledgeIDs(got))
	}
	// 通过（编辑后）、驳回；非待审核时冲突；external 不可审核
	if _, err := l.ApproveKnowledge(ctx, alice, fromRel.ID, nil); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("external approve: %v", err)
	}
	ap, err := l.ApproveKnowledge(ctx, rita, fromRel.ID, &lingo.KnowledgeDraft{Question: "导出报表为空（v1.2 前）", Answer: "升级到 v1.2"})
	if err != nil || ap.Status != lingo.KnowledgeActive || ap.Question != "导出报表为空（v1.2 前）" {
		t.Fatalf("approve = %+v %v", ap, err)
	}
	if _, err := l.ApproveKnowledge(ctx, rita, fromRel.ID, nil); !errors.Is(err, lingo.ErrConflict) {
		t.Errorf("approve twice: %v", err)
	}
	for _, e := range pend {
		if e.ID != fromRel.ID {
			if rj, err := l.RejectKnowledge(ctx, rita, e.ID); err != nil || rj.Status != lingo.KnowledgeRejected {
				t.Errorf("reject = %+v %v", rj, err)
			}
		}
	}
	if _, err := l.DisableKnowledge(ctx, rita, pend[0].ID); err != nil && !errors.Is(err, lingo.ErrConflict) {
		t.Errorf("disable rejected: %v", err)
	}
	// 每张工单只抽取一次
	runCtx, cancel = context.WithCancel(ctx)
	go func() { done <- l.Run(runCtx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source = 'ticket_extract'"); n != 2 {
		t.Errorf("extracted again: %d", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_llm_usage WHERE kind = 'knowledge_extract'"); n != 2 {
		t.Errorf("extract usage = %d", n)
	}
	// REST 审核
	rc := restClient{t: t, h: l.Handler()}
	if code, _, _ := rc.do("r1", "reviewer", "POST", fmt.Sprintf("/knowledge/%d/reject", fromRel.ID), map[string]any{}); code != 409 {
		t.Errorf("rest reject active: %d", code)
	}
}

// REQ-007：回答反馈（计数、只有知识类回答入候选、去编号、一条消息一个候选）、后台评论入库、未答上统计。
func TestKnowledgeFeedbackCommentsAndMisses(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	k, err := l.CreateKnowledge(ctx, rita, lingo.KnowledgeDraft{Project: "demo", Question: "怎么配置 SSO", Answer: "在集成页填写 IdP 元数据"})
	if err != nil {
		t.Fatal(err)
	}
	m.answer = func(data string) string {
		if strings.Contains(data, fmt.Sprintf("K%d", k.ID)) {
			return fmt.Sprintf("参考 K%d：在集成页填写 IdP 元数据，见 #7。", k.ID)
		}
		return "好的"
	}
	conv, _ := l.CreateConversation(ctx, bob, "")
	m.intents = []string{`{"intent":"knowledge_ask","keywords":["SSO"]}`}
	ans := send(t, l, bob, conv.ID, "SSO 怎么配置")

	if err := l.RateMessage(ctx, bob, conv.ID, ans.ID, true); err != nil {
		t.Fatal(err)
	}
	cands, _ := l.ListKnowledge(ctx, rita, lingo.KnowledgeFilter{Status: lingo.KnowledgePending})
	if len(cands) != 1 || cands[0].Source != lingo.KnowledgeSourceAssistantFeedback || cands[0].Question != "SSO 怎么配置" ||
		strings.Contains(cands[0].Answer, "K") || strings.Contains(cands[0].Answer, "#7") || cands[0].DuplicateOf != k.ID {
		t.Fatalf("candidates = %+v", cands)
	}
	got, _ := l.ListKnowledge(ctx, rita, lingo.KnowledgeFilter{Status: lingo.KnowledgeActive})
	if got[0].HelpfulCount != 1 || got[0].UnhelpfulCount != 0 {
		t.Errorf("counts after helpful = %+v", got[0])
	}
	// 改为没帮助：计数调整，不重复入候选；再改回有帮助也只有一个候选
	if err := l.RateMessage(ctx, bob, conv.ID, ans.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = l.ListKnowledge(ctx, rita, lingo.KnowledgeFilter{Status: lingo.KnowledgeActive})
	if got[0].HelpfulCount != 0 || got[0].UnhelpfulCount != 1 {
		t.Errorf("counts after switch = %+v", got[0])
	}
	if err := l.RateMessage(ctx, bob, conv.ID, ans.ID, true); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source = 'assistant_feedback'"); n != 1 {
		t.Errorf("candidates = %d", n)
	}
	// 非知识类回答：只记录；他人会话 not_found；用户消息 invalid
	tk := seedTicketRow(t, db, "u2", "", "api", "bug", "登录超时", "in_progress", 0)
	m.intents = []string{fmt.Sprintf(`{"intent":"ticket_find","ticket_ids":[%d]}`, tk)}
	m.answer = func(string) string { return fmt.Sprintf("#%d 处理中", tk) }
	tAns := send(t, l, bob, conv.ID, fmt.Sprintf("#%d 怎么样了", tk))
	if err := l.RateMessage(ctx, bob, conv.ID, tAns.ID, true); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source = 'assistant_feedback'"); n != 1 {
		t.Errorf("ticket answer became candidate")
	}
	if err := l.RateMessage(ctx, alice, conv.ID, ans.ID, true); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("other's conversation: %v", err)
	}
	if err := l.RateMessage(ctx, bob, conv.ID, ans.ReplyTo, true); !errors.Is(err, lingo.ErrInvalidInput) {
		t.Errorf("rate user message: %v", err)
	}
	rc := restClient{t: t, h: l.Handler()}
	if code, _, _ := rc.do("u2", "external", "POST", fmt.Sprintf("/conversations/%d/messages/%d/feedback", conv.ID, ans.ID), map[string]any{"helpful": true}); code != 204 && code != 200 {
		t.Errorf("rest feedback: %d", code)
	}
	if code, _, _ := rc.do("u2", "external", "POST", fmt.Sprintf("/conversations/%d/messages/%d/feedback", conv.ID, ans.ID), map[string]any{}); code != 400 {
		t.Errorf("rest feedback without helpful: %d", code)
	}
	mc := mcpClient{t: t, h: l.MCPHandler()}
	if out, ok := mc.call("u2", "external", "rate_message", map[string]any{"conversation_id": conv.ID, "message_id": ans.ID, "helpful": false}); !ok {
		t.Errorf("mcp rate: %v", out)
	}

	// 来源 e：后台评论勾选入库（直接生效）；external 勾选 forbidden；非法内容评论不发表
	fwd := seedTicketRow(t, db, "u1", "", "api", "bug", "打印失败", "in_progress", 31)
	if _, err := l.AddTicketCommentWith(ctx, rita, fwd, lingo.CommentInput{Body: "请升级驱动", Knowledge: &lingo.KnowledgeDraft{Question: "打印失败怎么办", Answer: "升级打印机驱动"}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source = 'ticket_comment' AND status = 'active' AND source_ticket_id = $1", fwd); n != 1 {
		t.Errorf("comment knowledge = %d", n)
	}
	// 与已有条目重合低于阈值：不标可能重复
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source_ticket_id = $1 AND duplicate_of IS NOT NULL", fwd); n != 0 {
		t.Errorf("unrelated entry marked duplicate")
	}
	if _, err := l.AddTicketCommentWith(ctx, alice, fwd, lingo.CommentInput{Body: "x", Knowledge: &lingo.KnowledgeDraft{}}); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("external comment to knowledge: %v", err)
	}
	before := countRows(t, db, "SELECT count(*) FROM lingo_ticket_comments WHERE ticket_id = $1", fwd)
	if _, err := l.AddTicketCommentWith(ctx, rita, fwd, lingo.CommentInput{Body: "y", Knowledge: &lingo.KnowledgeDraft{Question: strings.Repeat("问", 501)}}); !errors.Is(err, lingo.ErrInvalidInput) {
		t.Errorf("invalid comment knowledge: %v", err)
	}
	if after := countRows(t, db, "SELECT count(*) FROM lingo_ticket_comments WHERE ticket_id = $1", fwd); after != before {
		t.Errorf("comment posted despite invalid knowledge")
	}
	if code, _, _ := rc.do("r1", "reviewer", "POST", fmt.Sprintf("/tickets/%d/comments", fwd), map[string]any{"body": "重启试试",
		"knowledge": map[string]any{"question": "打印卡住怎么办", "answer": "重启打印服务"}}); code != 201 {
		t.Errorf("rest comment with knowledge: %d", code)
	}

	// 未答上：只记知识类无命中；相似提问归并；不同提问新增；reviewer 可见，external 不可
	m.answer = func(string) string { return "好的" }
	bobDemo := bob
	bobDemo.Projects = []string{"demo"} // 只能访问 demo：未答上记录只归 demo
	for _, q := range []string{"电子签章怎么开通", "电子签章怎么开通呢", "如何申请发票"} {
		m.intents = []string{`{"intent":"knowledge_ask","keywords":[]}`}
		send(t, l, bobDemo, conv.ID, q)
	}
	qs, err := l.ListUnansweredQuestions(ctx, rita, lingo.UnansweredFilter{})
	if err != nil || len(qs) != 2 || qs[0].Hits != 2 {
		t.Fatalf("unanswered = %+v %v", qs, err)
	}
	if _, err := l.ListUnansweredQuestions(ctx, bob, lingo.UnansweredFilter{}); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("external unanswered: %v", err)
	}
	other := rita
	other.Projects = []string{"other"}
	if qs, _ := l.ListUnansweredQuestions(ctx, other, lingo.UnansweredFilter{}); len(qs) != 0 {
		t.Errorf("other project sees misses: %+v", qs)
	}
	if _, err := db.Exec("UPDATE lingo_knowledge_misses SET last_at = now() - interval '91 days' WHERE hits = 1"); err != nil {
		t.Fatal(err)
	}
	if qs, _ := l.ListUnansweredQuestions(ctx, rita, lingo.UnansweredFilter{}); len(qs) != 1 {
		t.Errorf("expired misses listed: %+v", qs)
	}
	if code, _, arr := rc.do("r1", "reviewer", "GET", "/knowledge/unanswered", nil); code != 200 || len(arr) != 1 {
		t.Errorf("rest unanswered: %d %v", code, arr)
	}
}

// 迁移 0006 可增量执行；升级前已结案的 Bug / 需求工单登记为 skipped，不抽取。
func TestKnowledgeReviewMigrationIncremental(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP TABLE lingo_knowledge_misses, lingo_knowledge_extractions, lingo_assistant_feedback;
		DROP INDEX lingo_knowledge_pending_idx;
		ALTER TABLE lingo_knowledge DROP COLUMN source_message_id, DROP COLUMN duplicate_of, DROP COLUMN helpful_count, DROP COLUMN unhelpful_count;
		ALTER TABLE lingo_knowledge DROP CONSTRAINT lingo_knowledge_status_check, DROP CONSTRAINT lingo_knowledge_source_check;
		ALTER TABLE lingo_knowledge ADD CONSTRAINT lingo_knowledge_status_check CHECK (status IN ('active', 'disabled')),
			ADD CONSTRAINT lingo_knowledge_source_check CHECK (source IN ('manual', 'ticket_answer'));
		DELETE FROM lingo_schema_migrations WHERE name LIKE '0006_%'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO lingo_projects (key, name) VALUES ('demo', 'Demo');
		INSERT INTO lingo_tickets (user_id, user_name, user_email, project_id, type, title, body, status)
		SELECT 'u1', 'u1', '', id, 'bug', 'old', 'old', 'deployed' FROM lingo_projects WHERE key = 'demo'`); err != nil {
		t.Fatal(err)
	}
	if err := lingo.Migrate(ctx, db); err != nil {
		t.Fatalf("incremental Migrate: %v", err)
	}
	if st, err := lingo.CheckMigrations(ctx, db); err != nil || !st.Ready {
		t.Errorf("after incremental: %+v %v", st, err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge_extractions WHERE status = 'skipped'"); n != 1 {
		t.Errorf("backfilled skipped = %d", n)
	}
}

// 抽取失败退避重试；未配置模型时不登记、不抽取；Run 清理超过 90 天的未答上记录。
func TestKnowledgeExtractRetryNoModelAndPurge(t *testing.T) {
	ctx := context.Background()
	runFor := func(l *lingo.Lingo, d time.Duration, until func() bool) {
		t.Helper()
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- l.Run(runCtx) }()
		deadline := time.Now().Add(d)
		for (until == nil || !until()) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		cancel()
		<-done
	}

	// 未配置模型
	db0 := testDB(t, true)
	l0 := newBareLingo(t, db0, nil, nil)
	if _, err := l0.ListTriage(ctx, rita, lingo.TriageFilter{}); err != nil {
		t.Fatal(err)
	}
	seedTicketRow(t, db0, "u1", "", "api", "bug", "x", "released", 1)
	runFor(l0, time.Second, nil)
	if n := countRows(t, db0, "SELECT count(*) FROM lingo_knowledge_extractions"); n != 0 {
		t.Errorf("enqueued without model: %d", n)
	}

	// 模型失败一次：记录失败次数与原因，稍后重试，不入库
	m := &scriptModel{fail: 1, answer: func(string) string { return extractJSON }}
	l, db := newAssistantLingo(t, m)
	tk := seedTicketRow(t, db, "u1", "", "api", "bug", "导出报表为空", "deployed", 9)
	if _, err := db.Exec(`INSERT INTO lingo_knowledge_misses (projects, question, tokens, last_at) VALUES ('{demo}', '旧问题', '{旧问}', now() - interval '91 days')`); err != nil {
		t.Fatal(err)
	}
	runFor(l, 10*time.Second, func() bool {
		return countRows(t, db, "SELECT count(*) FROM lingo_knowledge_extractions WHERE ticket_id = $1 AND attempts = 1", tk) == 1
	})
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge_extractions WHERE ticket_id = $1 AND status = 'pending' AND attempts = 1 AND last_error <> '' AND locked_until > now()", tk); n != 1 {
		t.Errorf("retry not scheduled")
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge"); n != 0 {
		t.Errorf("entry stored after failure: %d", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge_misses"); n != 0 {
		t.Errorf("expired misses not purged: %d", n)
	}
}
