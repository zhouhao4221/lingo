package tests

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// REQ-012 独立评审第 1 轮补充的隔离测试（合成数据、假模型、只读假 Gitea）。

// B2：未启用策略时生成、引用了来源未知知识的 reviewer 回答（untrusted），启用后同一会话继续提问时正文与引用的知识都不交给
// 模型；只因内部评论为 authorized 的 reviewer 回答照常回放。
func TestModelContentHistoryScopes(t *testing.T) {
	db := testDB(t, true)
	g := &commentGitea{comments: map[string][]map[string]any{}}
	m := newRecModel()
	m.answer = func(data string) string {
		switch {
		case strings.Contains(data, "SYN-H-INTERNAL"):
			return "SYN-ANS-AUTH 内部评论显示正在修复"
		case strings.Contains(data, "SYN-H-LEGACY-A"):
			return "SYN-ANS-UNTRUSTED 参考旧知识处理"
		}
		return "好的"
	}
	off := newMCLingo(t, db, g, lingo.ReplyAll, lingo.ModelContentDefault, m)
	ctx := context.Background()
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	t1 := seedIssueTicket(t, db, "demo", "api", "u1", "in_progress", 12)
	g.set("demo/api#12", gcomment(101, "dev", "SYN-H-INTERNAL 内部讨论", at))
	if _, err := off.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO lingo_knowledge (project_id, question, answer, source, status, search_tokens)
		SELECT id, 'SYN-H-LEGACY-Q 扫描仪卡顿', 'SYN-H-LEGACY-A 旧抽取的处理办法', 'ticket_extract', 'active', '{扫描,描仪,卡顿}'
		FROM lingo_projects WHERE key = 'demo'`)

	c, _ := off.CreateConversation(ctx, rita, "")
	m.setIntent(fmt.Sprintf(`{"intent":"ticket_find","ticket_ids":[%d]}`, t1))
	a1 := send(t, off, rita, c.ID, "这个工单怎么样了")
	m.setIntent(`{"intent":"knowledge_ask","keywords":["扫描仪","卡顿"]}`)
	a2 := send(t, off, rita, c.ID, "扫描仪卡顿怎么办")
	if s1, s2 := messageScope(t, db, a1.ID), messageScope(t, db, a2.ID); s1 != "authorized" || s2 != "untrusted" {
		t.Fatalf("scopes = %q %q", s1, s2)
	}
	if len(a2.Citations.Knowledge) == 0 {
		t.Fatalf("legacy knowledge not cited: %+v", a2.Citations)
	}

	on := newMCLingo(t, db, g, lingo.ReplyAll, lingo.ModelContentMarked, m)
	m.reset()
	m.setIntent(`{"intent":"clarify"}`)
	a3 := send(t, on, rita, c.ID, "总结一下")
	contains(t, "reviewer history after enabling", m.sent(), []string{"SYN-ANS-AUTH"},
		[]string{"SYN-ANS-UNTRUSTED", "SYN-H-LEGACY-Q", "SYN-H-LEGACY-A"})
	if s := messageScope(t, db, a3.ID); s != "authorized" {
		t.Errorf("answer replaying authorized history scope = %q", s)
	}
	// 未启用时照常回放（行为不变），并如实记为 untrusted
	m.reset()
	m.setIntent(`{"intent":"clarify"}`)
	a4 := send(t, off, rita, c.ID, "再总结一下")
	contains(t, "reviewer history default", m.sent(), []string{"SYN-ANS-AUTH", "SYN-ANS-UNTRUSTED", "SYN-H-LEGACY-Q"}, nil)
	if s := messageScope(t, db, a4.ID); s != "untrusted" {
		t.Errorf("default replay scope = %q", s)
	}
	// 回答候选：authorized / untrusted 的回答都不是可信来源
	if err := on.RateMessage(ctx, rita, c.ID, a2.ID, true); err != nil {
		t.Fatal(err)
	}
	pend, _ := on.ListKnowledge(ctx, rita, lingo.KnowledgeFilter{Status: lingo.KnowledgePending})
	if len(pend) != 1 || pend[0].ModelInput == nil || *pend[0].ModelInput {
		t.Errorf("candidate from untrusted answer = %+v", pend)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source = 'assistant_feedback' AND model_ok = false"); n != 1 {
		t.Errorf("candidate provenance rows = %d", n)
	}
}

// T1：配置 TicketAudience 的同事、被合并工单的提交人——启用策略后展示范围与升级前一致，交给模型的只有准入评论；写权限、
// 附件、事件流不变。
func TestModelContentAudienceAndMerged(t *testing.T) {
	db := testDB(t, true)
	m := newRecModel()
	a := &audience{peers: map[string][]string{}}
	l := newMCLingoWith(t, db, &commentGitea{comments: map[string][]map[string]any{}}, lingo.ReplyAll, lingo.ModelContentMarked, m,
		func(c *lingo.Config) { c.TicketAudience = a.fn })
	ctx := context.Background()
	bobT, err := l.CreateTicket(ctx, bob, lingo.TicketInput{Project: "demo", Type: lingo.TicketBug, Title: "导出报表失败",
		Content: "导出报表时报错", Attachments: []lingo.Attachment{{ID: "shot", Name: "s.png", URL: "https://app.example.com/f/s"}}})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "UPDATE lingo_tickets SET status = 'in_progress', gitea_issue_number = 41 WHERE id = $1", bobT.ID)
	mustExec(t, db, `INSERT INTO lingo_ticket_comments (ticket_id, author_name, source, body, public, gitea_comment_id, model_marked) VALUES
		($1, 'dev', 'gitea', 'SYN-P-MARKED 对外回复', true, 401, true), ($1, 'dev', 'gitea', 'SYN-P-PLAIN 普通评论', true, 402, false)`, bobT.ID)
	// 合并：dave（u4）的工单并入 erin（u5）的主工单
	dave := lingo.User{ID: "u4", Name: "u4", Role: lingo.RoleExternal}
	target := seedIssueTicket(t, db, "demo", "api", "u5", "in_progress", 42)
	merged := seedIssueTicket(t, db, "demo", "api", "u4", "pending_triage", 0)
	mustExec(t, db, "UPDATE lingo_tickets SET status = 'merged', merged_into_ticket_id = $1 WHERE id = $2", target, merged)
	mustExec(t, db, `INSERT INTO lingo_ticket_comments (ticket_id, author_name, source, body, public, gitea_comment_id, model_marked) VALUES
		($1, 'dev', 'gitea', 'SYN-T-PLAIN 主工单普通评论', true, 501, false), ($1, 'dev', 'gitea', 'SYN-T-MARKED 主工单对外回复', true, 502, true)`, target)
	mustExec(t, db, `INSERT INTO lingo_ticket_comments (ticket_id, author_id, author_name, source, body, public) VALUES
		($1, 'u4', 'u4', 'user', 'SYN-M-LOCAL 被合并工单的补充', true)`, merged)
	bindMarked(t, db)
	a.set("u1", "u2")

	// 同事：展示全部对外评论（ReplyAll），模型只收到准入的
	cs, err := l.ListTicketComments(ctx, alice, bobT.ID)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "peer visible", strings.Join(commentBodies(cs), "|"), []string{"SYN-P-MARKED", "SYN-P-PLAIN"}, nil)
	s, _ := askTicket(t, l, m, alice, bobT.ID)
	contains(t, "peer model input", s, []string{"SYN-P-MARKED"}, []string{"SYN-P-PLAIN"})
	if _, err := l.AddTicketComment(ctx, alice, bobT.ID, "我也遇到了", nil); err == nil {
		t.Error("peer can comment")
	}
	rc := restClient{t: t, h: l.Handler()}
	if code, obj, _ := rc.do("u1", "external", "GET", fmt.Sprintf("/tickets/%d", bobT.ID), nil); code != 200 {
		t.Errorf("peer detail: %d", code)
	} else if as, _ := obj["attachments"].([]any); len(as) != 0 {
		t.Errorf("peer sees attachments: %v", as)
	}
	if code, obj, _ := rc.do("u2", "external", "GET", fmt.Sprintf("/tickets/%d", bobT.ID), nil); code != 200 {
		t.Errorf("owner detail: %d", code)
	} else if as, _ := obj["attachments"].([]any); len(as) != 1 {
		t.Errorf("owner attachments: %v", as)
	}
	evs, _ := l.ListEvents(ctx, alice, 0, 200)
	for _, e := range evs {
		if e.TicketID == bobT.ID {
			t.Errorf("peer event leaked: %+v", e)
		}
	}
	// 被合并工单的提交人：看自己的工单（含补充），主工单仍不可见，模型也收不到主工单的评论
	cs, err = l.ListTicketComments(ctx, dave, merged)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "merged visible", strings.Join(commentBodies(cs), "|"), []string{"SYN-M-LOCAL"}, []string{"SYN-T-"})
	if _, err := l.ListTicketComments(ctx, dave, target); err == nil {
		t.Error("merge target visible to merged submitter")
	}
	s, _ = askTicket(t, l, m, dave, merged)
	contains(t, "merged model input", s, []string{"SYN-M-LOCAL"}, []string{"SYN-T-PLAIN", "SYN-T-MARKED"})
	s, _ = askTicket(t, l, m, dave, target)
	contains(t, "merge target model input", s, nil, []string{"SYN-T-PLAIN", "SYN-T-MARKED"})
}

// T2：启用策略时 ResyncTicketComments 的 Notify=true 契约不变；只改变模型判定的写入不发事件、不留痕。
func TestModelContentResyncNotifyContract(t *testing.T) {
	db := testDB(t, true)
	g := &commentGitea{comments: map[string][]map[string]any{}}
	m := newRecModel()
	l := newMCLingo(t, db, g, lingo.ReplyAll, lingo.ModelContentMarked, m)
	ctx := context.Background()
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	t1 := seedIssueTicket(t, db, "demo", "api", "u1", "in_progress", 12)
	g.set("demo/api#12", gcomment(101, "dev", "SYN-N-PLAIN 普通评论", at), gcomment(102, "dev", "/reply SYN-N-MARKED 对外回复", at))
	hist0 := countRows(t, db, "SELECT count(*) FROM lingo_ticket_events WHERE ticket_id = $1", t1)
	r, err := l.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{Notify: true})
	if err != nil || r.Added != 2 || r.Published != 2 || !r.Notified {
		t.Fatalf("notify resync = %+v %v", r, err)
	}
	evs, _ := l.ListEvents(ctx, alice, 0, 200)
	var bodies []string
	for _, e := range evs {
		if e.Kind == lingo.EventTicketComment && e.TicketID == t1 {
			bodies = append(bodies, e.Body)
		}
	}
	slices.Sort(bodies)
	if len(bodies) != 2 || !strings.Contains(bodies[0], "SYN-N-") || strings.Contains(strings.Join(bodies, "|"), "/reply") {
		t.Errorf("notify events = %v", bodies)
	}
	hist1 := countRows(t, db, "SELECT count(*) FROM lingo_ticket_events WHERE ticket_id = $1", t1)
	if hist1-hist0 != 2 {
		t.Errorf("audit rows = %d", hist1-hist0)
	}
	// 只改变模型判定（如升级前同步的评论首次判定）：不发事件、不留痕，计为 Unchanged
	mustExec(t, db, "UPDATE lingo_ticket_comments SET model_marked = NULL, model_marked_at = NULL, model_marked_md5 = NULL WHERE ticket_id = $1", t1)
	evN := len(evs)
	r, err = l.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{Notify: true})
	if err != nil || r.Unchanged != 2 || r.Published != 0 || r.Notified {
		t.Fatalf("classify-only resync = %+v %v", r, err)
	}
	if evs, _ := l.ListEvents(ctx, alice, 0, 200); len(evs) != evN {
		t.Errorf("classify-only emitted events: %d → %d", evN, len(evs))
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_ticket_events WHERE ticket_id = $1", t1); n != hist1 {
		t.Errorf("classify-only left history: %d → %d", hist1, n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_ticket_comments WHERE ticket_id = $1 AND model_marked IS NOT NULL", t1); n != 2 {
		t.Errorf("classification not stored: %d", n)
	}
	s, _ := askTicket(t, l, m, alice, t1)
	contains(t, "after classify", s, []string{"SYN-N-MARKED"}, []string{"SYN-N-PLAIN"})
}

// T3：并发的重新抽取（及与自动抽取同时）最多一条可信候选，另一方返回 exists，不重复入库、不额外调用模型；
// 自动抽取遇到已有抽取条目的工单不调用模型。
func TestModelContentReextractConcurrent(t *testing.T) {
	db := testDB(t, true)
	m := newRecModel()
	var calls sync.Mutex
	n := 0
	m.answer = func(data string) string {
		if strings.Contains(data, `"closed"`) {
			calls.Lock()
			n++
			calls.Unlock()
			time.Sleep(300 * time.Millisecond) // 让两次调用重叠
			return `{"skip":false,"question":"打印机卡纸怎么办","answer":"SYN-C-ANSWER 取出纸张"}`
		}
		return "好的"
	}
	l := newMCLingo(t, db, &commentGitea{comments: map[string][]map[string]any{}}, lingo.ReplyAll, lingo.ModelContentMarked, m)
	ctx := context.Background()
	seed := func(issue int64, extraction string) int64 {
		id := seedIssueTicket(t, db, "demo", "api", "u1", "released", issue)
		mustExec(t, db, "INSERT INTO lingo_knowledge_extractions (ticket_id, status) VALUES ($1, $2)", id, extraction)
		mustExec(t, db, `INSERT INTO lingo_knowledge (project_id, question, answer, source, source_ticket_id, status)
			SELECT id, 'q', 'SYN-C-OLD', 'ticket_extract', $1, 'active' FROM lingo_projects WHERE key = 'demo'`, id)
		return id
	}
	// 1. 两次重新抽取同时执行
	t1 := seed(51, "done")
	var wg sync.WaitGroup
	outs := make([]string, 2)
	ids := make([]int64, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.ReextractTicketKnowledge(ctx, rita, t1)
			if err != nil {
				t.Errorf("concurrent reextract %d: %v", i, err)
				return
			}
			outs[i] = r.Outcome
			if r.Entry != nil {
				ids[i] = r.Entry.ID
			}
		}()
	}
	wg.Wait()
	slices.Sort(outs)
	if outs[0] != lingo.KnowledgeReextractCreated || outs[1] != lingo.KnowledgeReextractExists || ids[0] != ids[1] || ids[0] == 0 {
		t.Errorf("concurrent outcomes = %v ids %v", outs, ids)
	}
	if c := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source_ticket_id = $1 AND model_ok", t1); c != 1 {
		t.Errorf("trusted candidates = %d", c)
	}
	if n != 1 {
		t.Errorf("model calls = %d", n)
	}
	// 2. 与自动抽取同时：工单的抽取登记仍为 pending
	t2 := seed(52, "pending")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- l.Run(runCtx) }()
	r, err := l.ReextractTicketKnowledge(ctx, rita, t2)
	if err != nil || (r.Outcome != lingo.KnowledgeReextractCreated && r.Outcome != lingo.KnowledgeReextractExists) {
		t.Errorf("reextract during auto = %+v %v", r, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// 自动抽取每 30 秒一轮：重启 Run 立即再跑一轮（重新抽取持有租约时，自动抽取领取不到）
	runCtx, cancel = context.WithCancel(ctx)
	go func() { done <- l.Run(runCtx) }()
	mcWait(t, "auto extraction done", func() bool {
		return countRows(t, db, "SELECT count(*) FROM lingo_knowledge_extractions WHERE ticket_id = $1 AND status = 'done'", t2) == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source_ticket_id = $1 AND model_ok", t2); c != 1 {
		t.Errorf("trusted candidates with auto = %d", c)
	}
	if c := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source_ticket_id = $1", t2); c != 2 {
		t.Errorf("rows with auto = %d", c)
	}
	if n != 2 {
		t.Errorf("model calls with auto = %d (auto extraction must skip tickets that already have an extract)", n)
	}
	if c := countRows(t, db, "SELECT count(*) FROM lingo_knowledge_extractions WHERE locked_until IS NOT NULL"); c != 0 {
		t.Errorf("leases left: %d", c)
	}
}

// T4：未启用策略时，送给向量模型的文本、用户检索结果与历史回放与升级前逐字一致（含来源未知的条目与回答）。
func TestModelContentDefaultVerbatim(t *testing.T) {
	db := testDB(t, true)
	m := newRecModel()
	l := newMCLingo(t, db, &commentGitea{comments: map[string][]map[string]any{}}, lingo.ReplyAll, lingo.ModelContentDefault, m)
	ctx := context.Background()
	if _, err := l.CreateKnowledge(ctx, rita, lingo.KnowledgeDraft{Project: "demo", Question: "打印机卡纸怎么办", Answer: "SYN-V-MANUAL 打开盖板"}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO lingo_knowledge (project_id, question, answer, source, status, search_tokens)
		SELECT id, '打印机卡纸处理', 'SYN-V-LEGACY 旧抽取', 'ticket_extract', 'active', '{打印,印机,卡纸,处理}' FROM lingo_projects WHERE key = 'demo'`)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- l.Run(runCtx) }()
	mcWait(t, "embeddings", func() bool {
		return countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE embedding IS NULL") == 0
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m.rmu.Lock()
	embeds := slices.Clone(m.embeds)
	m.rmu.Unlock()
	slices.Sort(embeds)
	want := []string{"打印机卡纸处理\nSYN-V-LEGACY 旧抽取", "打印机卡纸怎么办\nSYN-V-MANUAL 打开盖板"}
	if !slices.Equal(embeds, want) {
		t.Errorf("embedded texts = %q, want %q", embeds, want)
	}
	got, err := l.SearchKnowledge(ctx, alice, "打印机卡纸", "", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("search = %+v %v", got, err)
	}
	var answers []string
	for _, e := range got {
		answers = append(answers, e.Answer)
		if e.ModelInput != nil {
			t.Errorf("model_input in user search: %+v", e)
		}
	}
	slices.Sort(answers)
	if !slices.Equal(answers, []string{"SYN-V-LEGACY 旧抽取", "SYN-V-MANUAL 打开盖板"}) {
		t.Errorf("search answers = %q", answers)
	}
	c, _ := l.CreateConversation(ctx, alice, "")
	mustExec(t, db, `INSERT INTO lingo_assistant_messages (conversation_id, role, content, status)
		VALUES ($1, 'user', 'SYN-V-Q0 之前的问题', 'done'), ($1, 'assistant', 'SYN-V-HIST 旧回答原文', 'done')`, c.ID)
	m.reset()
	m.setIntent(`{"intent":"knowledge_ask","keywords":["打印机","卡纸"]}`)
	send(t, l, alice, c.ID, "打印机卡纸怎么办")
	m.rmu.Lock()
	chats := slices.Clone(m.chats)
	m.rmu.Unlock()
	if !slices.Contains(chats, "SYN-V-HIST 旧回答原文") || !slices.Contains(chats, "SYN-V-Q0 之前的问题") {
		t.Errorf("history not replayed verbatim: %q", chats)
	}
	contains(t, "default knowledge facts", strings.Join(chats, "\n"), []string{"SYN-V-LEGACY 旧抽取", "SYN-V-MANUAL 打开盖板"}, []string{"已省略"})
}
