package tests

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// REQ-012（待办 #793）：模型内容策略 ModelContentMarked 的隔离测试。全部为合成数据：假模型只记录收到的内容，
// 假 Gitea 只读，不发真实通知。

// recModel 在 scriptModel 的基础上记录每次 Chat 收到的全部消息内容与 Embed 收到的文本（即“正常出站数据”）。
type recModel struct {
	*scriptModel
	rmu    sync.Mutex
	chats  []string
	embeds []string
}

func newRecModel() *recModel { return &recModel{scriptModel: &scriptModel{}} }

func (m *recModel) Chat(ctx context.Context, ms []lingo.Message) (string, error) {
	m.rmu.Lock()
	for _, x := range ms {
		m.chats = append(m.chats, x.Content)
	}
	m.rmu.Unlock()
	return m.scriptModel.Chat(ctx, ms)
}

func (m *recModel) Embed(_ context.Context, texts []string) ([][]float32, error) {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	m.embeds = append(m.embeds, texts...)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (m *recModel) EmbeddingModelName() string { return "emb-1" }

// sent 是 reset 之后发给模型（对话与向量）的全部内容。
func (m *recModel) sent() string {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	return strings.Join(m.chats, "\n") + "\n" + strings.Join(m.embeds, "\n")
}

func (m *recModel) reset() {
	m.rmu.Lock()
	m.chats, m.embeds = nil, nil
	m.rmu.Unlock()
}

func (m *recModel) setIntent(s string) {
	m.mu.Lock()
	m.intents = []string{s}
	m.mu.Unlock()
}

func newMCLingo(t *testing.T, db *sql.DB, g http.Handler, mode lingo.ReplyMode, policy lingo.ModelContentPolicy, m lingo.Model) *lingo.Lingo {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: m, ModelContent: policy, SkipSelfCheck: true, ReconcileInterval: -1,
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s", ReplyMode: mode},
		Projects: []lingo.Project{
			{Key: "demo", Name: "Demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}, {Owner: "demo", Name: "web"}}},
			{Key: "other", Name: "Other", Repos: []lingo.Repo{{Owner: "other", Name: "svc"}}}},
		Authenticate: headerUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.ListTriage(context.Background(), rita, lingo.TriageFilter{}); err != nil { // 同步项目入库
		t.Fatal(err)
	}
	return l
}

// askTicket 以 u 的身份在新会话中问工单 id 的进展，返回本轮发给模型的全部内容与助手消息。
func askTicket(t *testing.T, l *lingo.Lingo, m *recModel, u lingo.User, id int64) (string, lingo.ConversationMessage) {
	t.Helper()
	c, err := l.CreateConversation(context.Background(), u, "")
	if err != nil {
		t.Fatal(err)
	}
	m.reset()
	m.setIntent(fmt.Sprintf(`{"intent":"ticket_find","ticket_ids":[%d]}`, id))
	msg := send(t, l, u, c.ID, "这个工单进展如何")
	return m.sent(), msg
}

func messageModelOK(t *testing.T, db *sql.DB, id int64) sql.NullBool {
	t.Helper()
	var v sql.NullBool
	if err := db.QueryRow("SELECT model_ok FROM lingo_assistant_messages WHERE id = $1", id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func contains(t *testing.T, what, s string, want []string, not []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Errorf("%s: missing %q", what, w)
		}
	}
	for _, n := range not {
		if strings.Contains(s, n) {
			t.Errorf("%s: must not contain %q", what, n)
		}
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func mcWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 普通用户问答只把 ReplyMarked 内容范围内的评论交给模型；用户可见范围仍按 ReplyMode（ReplyAll 全部非机器人评论）；
// reviewer / agent 的已授权范围不变；历史回补、编辑、乱序旧版本、Webhook 创建 / 编辑 / 删除一致。
func TestModelContentMarkedComments(t *testing.T) {
	db := testDB(t, true)
	g := &commentGitea{comments: map[string][]map[string]any{}}
	m := newRecModel()
	l := newMCLingo(t, db, g, lingo.ReplyAll, lingo.ModelContentMarked, m)
	ctx := context.Background()
	at := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	t1 := seedIssueTicket(t, db, "demo", "api", "u1", "in_progress", 12)
	// 升级前（ReplyAll）同步的历史评论：对外、模型准入未判定；灵工本地的提交人补充
	mustExec(t, db, `INSERT INTO lingo_ticket_comments (ticket_id, author_name, source, body, public, gitea_comment_id, created_at, updated_at)
		VALUES ($1, 'dev', 'gitea', 'SYN-LEGACY 旧评论', true, 100, $2, $2)`, t1, at)
	mustExec(t, db, `INSERT INTO lingo_ticket_comments (ticket_id, author_id, author_name, source, body, public, created_at, updated_at)
		VALUES ($1, 'u1', 'Alice', 'user', 'SYN-LOCAL 用户补充', true, $2, $2)`, t1, at)
	g.set("demo/api#12",
		gcomment(100, "dev", "SYN-LEGACY 旧评论", at),
		gcomment(101, "dev", "/reply SYN-MARKED-A 已定位", at),
		gcomment(102, "dev", "SYN-INTERNAL-B 内部讨论", at),
		gcomment(103, "lingo-bot", "SYN-BOT 灵工转发", at))

	// 1. 回补前：来源未知的历史评论不交给模型，但提交人照常可见
	s, msg := askTicket(t, l, m, alice, t1)
	contains(t, "before resync", s, []string{"SYN-LOCAL"}, []string{"SYN-LEGACY", "SYN-INTERNAL", "SYN-BOT"})
	if v := messageModelOK(t, db, msg.ID); !v.Valid || !v.Bool {
		t.Errorf("external answer model_ok = %+v", v)
	}
	cs, _ := l.ListTicketComments(ctx, alice, t1)
	contains(t, "visible before", strings.Join(commentBodies(cs), "|"), []string{"SYN-LEGACY", "SYN-LOCAL"}, nil)

	// 2. 静默历史回补：可见性按 ReplyAll，模型准入按原始标记；默认不通知
	evBefore, _ := l.ListEvents(ctx, alice, 0, 200)
	r, err := l.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{})
	if err != nil || r.Added != 2 || r.Unchanged != 1 || r.Bot != 1 || r.Published != 2 || r.Notified {
		t.Fatalf("resync = %+v %v", r, err)
	}
	if evAfter, _ := l.ListEvents(ctx, alice, 0, 200); len(evAfter) != len(evBefore) {
		t.Errorf("silent resync emitted events: %d → %d", len(evBefore), len(evAfter))
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_ticket_comments WHERE model_marked IS NULL AND source = 'gitea'"); n != 0 {
		t.Errorf("unclassified gitea comments after resync: %d", n)
	}
	cs, _ = l.ListTicketComments(ctx, alice, t1)
	contains(t, "visible after", strings.Join(commentBodies(cs), "|"), []string{"SYN-LEGACY", "SYN-MARKED-A", "SYN-INTERNAL-B", "SYN-LOCAL"}, []string{"SYN-BOT", "/reply"})
	s, _ = askTicket(t, l, m, alice, t1)
	contains(t, "external after resync", s, []string{"SYN-MARKED-A", "SYN-LOCAL"}, []string{"SYN-LEGACY", "SYN-INTERNAL-B", "/reply"})
	// 同事（TicketAudience 未配置）看不到他人工单；reviewer / agent 已授权的范围不变
	s, msg = askTicket(t, l, m, rita, t1)
	contains(t, "reviewer", s, []string{"SYN-MARKED-A", "SYN-INTERNAL-B", "SYN-LEGACY", "SYN-LOCAL"}, nil)
	if v := messageModelOK(t, db, msg.ID); !v.Valid || v.Bool {
		t.Errorf("reviewer answer with internal comments model_ok = %+v", v)
	}
	s, _ = askTicket(t, l, m, agent, t1)
	contains(t, "agent", s, []string{"SYN-INTERNAL-B"}, nil)
	s, _ = askTicket(t, l, m, bob, t1)
	contains(t, "other user", s, nil, []string{"SYN-MARKED-A", "SYN-LOCAL", "SYN-INTERNAL-B"})

	// 3. 编辑：标记去掉后不再准入（仍可见），加上标记后准入；乱序旧版本不覆盖；重复回补幂等
	g.set("demo/api#12",
		gcomment(100, "dev", "SYN-LEGACY 旧评论", at),
		gcomment(101, "dev", "SYN-EDIT-C 改为普通评论", at.Add(time.Hour)),
		gcomment(102, "dev", "/reply SYN-MARKED-D 公开说明", at.Add(time.Hour)))
	if r, err := l.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{}); err != nil || r.Updated != 2 || r.Published != 0 || r.Unpublished != 0 {
		t.Fatalf("edit resync = %+v %v", r, err)
	}
	s, _ = askTicket(t, l, m, alice, t1)
	contains(t, "after edit", s, []string{"SYN-MARKED-D"}, []string{"SYN-EDIT-C", "SYN-MARKED-A", "SYN-INTERNAL-B"})
	g.set("demo/api#12", gcomment(102, "dev", "SYN-STALE-E 旧版本", at))
	if r, err := l.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{}); err != nil || r.Unchanged != 1 {
		t.Fatalf("stale resync = %+v %v", r, err)
	}
	s, _ = askTicket(t, l, m, alice, t1)
	contains(t, "after stale", s, []string{"SYN-MARKED-D"}, []string{"SYN-STALE-E"})
	g.set("demo/api#12",
		gcomment(100, "dev", "SYN-LEGACY 旧评论", at),
		gcomment(101, "dev", "SYN-EDIT-C 改为普通评论", at.Add(time.Hour)),
		gcomment(102, "dev", "/reply SYN-MARKED-D 公开说明", at.Add(time.Hour)))
	hist := countRows(t, db, "SELECT count(*) FROM lingo_ticket_events WHERE ticket_id = $1", t1)
	if r, err := l.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{}); err != nil || r.Unchanged != 3 {
		t.Fatalf("idempotent resync = %+v %v", r, err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_ticket_events WHERE ticket_id = $1", t1); n != hist {
		t.Errorf("idempotent resync added history: %d → %d", hist, n)
	}

	// 4. Webhook：创建、编辑、删除与回补一致；通知按 ReplyMode 照常
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- l.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	issue := `"issue":{"id":112,"number":12,"user":{"login":"lingo-bot"},"body":"x","updated_at":"2026-11-18T01:00:00Z"},"repository":{"full_name":"demo/api"}`
	hook := func(n int, action string, id int64, body string, upd time.Time) {
		t.Helper()
		p := fmt.Sprintf(`{"action":%q,%s,"comment":{"id":%d,"body":%q,"user":{"login":"dev"},"updated_at":%q},"sender":{"login":"dev"}}`,
			action, issue, id, body, upd.Format(time.RFC3339))
		if w := deliver(l.WebhookHandler(), fmt.Sprintf("mc-d%d", n), "issue_comment", p, "s"); w.Code != 200 {
			t.Fatalf("deliver: %d %s", w.Code, w.Body)
		}
	}
	marked := func(id int64) sql.NullBool {
		var v sql.NullBool
		_ = db.QueryRow("SELECT model_marked FROM lingo_ticket_comments WHERE gitea_comment_id = $1 AND deleted_at IS NULL", id).Scan(&v)
		return v
	}
	now := time.Now().UTC().Truncate(time.Second)
	hook(1, "created", 104, "SYN-WH-INTERNAL 普通评论", now)
	hook(2, "created", 105, "/reply SYN-WH-MARKED 对外回复", now)
	mcWait(t, "webhook created", func() bool { return marked(104).Valid && marked(105).Valid })
	if marked(104).Bool || !marked(105).Bool {
		t.Errorf("webhook model_marked = %v %v", marked(104), marked(105))
	}
	hook(3, "edited", 104, "/reply SYN-WH-MARKED2 改为对外", now.Add(time.Minute))
	mcWait(t, "webhook edited", func() bool { return marked(104).Bool })
	hook(4, "deleted", 102, "/reply SYN-MARKED-D 公开说明", at.Add(time.Hour))
	mcWait(t, "webhook deleted", func() bool {
		return countRows(t, db, "SELECT count(*) FROM lingo_ticket_comments WHERE gitea_comment_id = 102 AND deleted_at IS NOT NULL") == 1
	})
	s, _ = askTicket(t, l, m, alice, t1)
	contains(t, "after webhook", s, []string{"SYN-WH-MARKED", "SYN-WH-MARKED2"}, []string{"SYN-WH-INTERNAL", "SYN-MARKED-D"})
	evs, _ := l.ListEvents(ctx, alice, 0, 200)
	var notified []string
	for _, e := range evs {
		if e.Kind == lingo.EventTicketComment {
			notified = append(notified, e.Body)
		}
	}
	// ReplyAll：新的普通评论照常通知提交人（通知机制不变）
	contains(t, "webhook notifications", strings.Join(notified, "|"), []string{"SYN-WH-MARKED", "对外回复"}, nil)
	if len(notified) != 2 {
		t.Errorf("notified = %v", notified)
	}
}

// 派生内容（知识条目、向量、历史回答、回答候选）的防御性最小化；原记录与用户展示不变。
func TestModelContentDerivedContent(t *testing.T) {
	db := testDB(t, true)
	m := newRecModel()
	l := newMCLingo(t, db, &commentGitea{comments: map[string][]map[string]any{}}, lingo.ReplyAll, lingo.ModelContentMarked, m)
	ctx := context.Background()
	manual, err := l.CreateKnowledge(ctx, rita, lingo.KnowledgeDraft{Project: "demo", Question: "打印机卡纸怎么办", Answer: "SYN-MANUAL 打开盖板取出纸张"})
	if err != nil {
		t.Fatal(err)
	}
	src := seedIssueTicket(t, db, "demo", "api", "u1", "released", 31)
	src2 := seedIssueTicket(t, db, "demo", "api", "u1", "released", 32)
	mustExec(t, db, `INSERT INTO lingo_knowledge_extractions (ticket_id, status) VALUES ($1, 'done'), ($2, 'done')`, src, src2)
	mustExec(t, db, `INSERT INTO lingo_knowledge (project_id, question, answer, source, source_ticket_id, status, search_tokens, model_ok)
		SELECT id, '打印机卡纸处理', 'SYN-LEGACY-EXTRACT 旧抽取', 'ticket_extract', $1, 'active', '{打印,印机,卡纸,处理}', NULL FROM lingo_projects WHERE key = 'demo'`, src)
	mustExec(t, db, `INSERT INTO lingo_knowledge (project_id, question, answer, source, source_ticket_id, status, search_tokens, model_ok)
		SELECT id, '打印机卡纸处理', 'SYN-TRUSTED-EXTRACT 可信抽取', 'ticket_extract', $1, 'active', '{打印,印机,卡纸,处理}', true FROM lingo_projects WHERE key = 'demo'`, src2)
	mustExec(t, db, `INSERT INTO lingo_knowledge (project_id, question, answer, source, status, search_tokens, embedding, embed_model)
		SELECT id, '打印机卡纸', 'SYN-LEGACY-FB 旧回答候选', 'assistant_feedback', 'active', '{打印,印机,卡纸}', '{1,0}', 'emb-1' FROM lingo_projects WHERE key = 'demo'`)

	// 1. 向量只为来源可信的条目生成；旧条目已有的向量保留
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- l.Run(runCtx) }()
	mcWait(t, "embeddings", func() bool {
		return countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE embedding IS NOT NULL AND (source = 'manual' OR model_ok)") == 2
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	contains(t, "embedded", m.sent(), []string{"SYN-MANUAL", "SYN-TRUSTED-EXTRACT"}, []string{"SYN-LEGACY"})
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE answer LIKE 'SYN-LEGACY-EXTRACT%' AND embedding IS NULL"); n != 1 {
		t.Errorf("legacy extract embedded")
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE answer LIKE 'SYN-LEGACY-FB%' AND embedding IS NOT NULL"); n != 1 {
		t.Errorf("legacy embedding removed")
	}

	// 2. 问答检索只交给模型来源可信的条目；用户检索与维护列表照常展示全部，并标出 model_input
	c, _ := l.CreateConversation(ctx, alice, "")
	m.reset()
	m.setIntent(`{"intent":"knowledge_ask","keywords":["打印机","卡纸"]}`)
	ans := send(t, l, alice, c.ID, "SYN-USER-Q 打印机卡纸怎么办")
	contains(t, "knowledge answer", m.sent(), []string{"SYN-MANUAL", "SYN-TRUSTED-EXTRACT"}, []string{"SYN-LEGACY"})
	if v := messageModelOK(t, db, ans.ID); !v.Valid || !v.Bool {
		t.Errorf("knowledge answer model_ok = %+v", v)
	}
	if got, err := l.SearchKnowledge(ctx, alice, "打印机卡纸", "", 10); err != nil || len(got) != 4 {
		t.Errorf("user-visible search = %v %v", knowledgeIDs(got), err)
	}
	es, _ := l.ListKnowledge(ctx, rita, lingo.KnowledgeFilter{})
	for _, e := range es {
		if want := !strings.HasPrefix(e.Answer, "SYN-LEGACY"); e.ModelInput != want {
			t.Errorf("model_input %q = %v", e.Answer, e.ModelInput)
		}
		if e.Status != lingo.KnowledgeActive {
			t.Errorf("status changed: %+v", e)
		}
	}

	// 3. 历史：升级前的回答（来源未知）不作为模型输入，用户自己的提问保留；新回答记为可信
	var legacyID int64
	if err := db.QueryRow(`INSERT INTO lingo_assistant_messages (conversation_id, role, content, status, intent, outcome, citations, reply_to)
		VALUES ($1, 'assistant', 'SYN-LEGACY-HIST 旧回答', 'done', 'knowledge_ask', 'answered', $2, $3) RETURNING id`,
		c.ID, fmt.Sprintf(`{"tickets":[],"releases":[],"knowledge":[{"id":%d,"question":"q"}]}`, manual.ID), ans.ID-1).Scan(&legacyID); err != nil {
		t.Fatal(err)
	}
	m.reset()
	m.setIntent(`{"intent":"knowledge_ask","keywords":["打印机"]}`)
	next := send(t, l, alice, c.ID, "还有别的办法吗")
	contains(t, "history", m.sent(), []string{"SYN-USER-Q"}, []string{"SYN-LEGACY-HIST"})
	if v := messageModelOK(t, db, next.ID); !v.Valid || !v.Bool {
		t.Errorf("answer after legacy history model_ok = %+v", v)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_assistant_messages WHERE id = $1 AND content = 'SYN-LEGACY-HIST 旧回答' AND model_ok IS NULL", legacyID); n != 1 {
		t.Errorf("legacy message modified")
	}
	// reviewer 历史中含内部内容的回答（false）属于其已授权范围，照常带上；external 的不带
	for _, u := range []lingo.User{rita, alice} {
		cv, _ := l.CreateConversation(ctx, u, "")
		if _, err := db.Exec(`INSERT INTO lingo_assistant_messages (conversation_id, role, content, status, model_ok)
			VALUES ($1, 'user', 'q0', 'done', NULL), ($1, 'assistant', 'SYN-SCOPED-HIST 含内部评论的回答', 'done', false)`, cv.ID); err != nil {
			t.Fatal(err)
		}
		m.reset()
		m.setIntent(`{"intent":"clarify"}`)
		send(t, l, u, cv.ID, "继续")
		if got := strings.Contains(m.sent(), "SYN-SCOPED-HIST"); got != (u.Role == lingo.RoleReviewer) {
			t.Errorf("%s history includes scoped answer = %v", u.Role, got)
		}
	}

	// 4. 回答候选：可信回答的候选可信；升级前回答的候选来源未知（照常待审核、展示，不作为模型输入）
	if err := l.RateMessage(ctx, alice, c.ID, ans.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := l.RateMessage(ctx, alice, c.ID, legacyID, true); err != nil {
		t.Fatal(err)
	}
	pend, _ := l.ListKnowledge(ctx, rita, lingo.KnowledgeFilter{Status: lingo.KnowledgePending})
	if len(pend) != 2 {
		t.Fatalf("pending candidates = %+v", pend)
	}
	for _, e := range pend {
		if want := !strings.Contains(e.Answer, "SYN-LEGACY-HIST"); e.Source != lingo.KnowledgeSourceAssistantFeedback || e.ModelInput != want {
			t.Errorf("candidate %q: source %s model_input %v", e.Answer, e.Source, e.ModelInput)
		}
		if _, err := l.ApproveKnowledge(ctx, rita, e.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	m.reset()
	m.setIntent(`{"intent":"knowledge_ask","keywords":["打印机"]}`)
	c2, _ := l.CreateConversation(ctx, alice, "")
	send(t, l, alice, c2.ID, "打印机卡纸怎么办")
	contains(t, "approved legacy candidate", m.sent(), nil, []string{"SYN-LEGACY-HIST"})
}

// 来源未知的旧抽取知识按新规则手动重新抽取（方案 A）：只用 ReplyMarked 范围内的评论、旧条目不改、新候选走正常审核；
// 自动抽取同样只用范围内的评论；权限与冲突。
func TestModelContentReextract(t *testing.T) {
	db := testDB(t, true)
	m := newRecModel()
	m.answer = func(data string) string {
		if strings.Contains(data, `"closed"`) {
			return `{"skip":false,"question":"打印机卡纸怎么办","answer":"SYN-NEW-ANSWER 取出纸张后重启"}`
		}
		return "好的"
	}
	l := newMCLingo(t, db, &commentGitea{comments: map[string][]map[string]any{}}, lingo.ReplyAll, lingo.ModelContentMarked, m)
	ctx := context.Background()
	rel := seedIssueTicket(t, db, "demo", "api", "u1", "released", 21)
	open := seedIssueTicket(t, db, "demo", "api", "u1", "in_progress", 22)
	plain := seedIssueTicket(t, db, "demo", "api", "u1", "deployed", 23)
	for _, id := range []int64{rel, open, plain} {
		if _, err := db.Exec(`INSERT INTO lingo_knowledge_extractions (ticket_id, status) VALUES ($1, 'done') ON CONFLICT DO NOTHING`, id); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, db, `INSERT INTO lingo_ticket_comments (ticket_id, author_id, author_name, source, body, public)
		VALUES ($1, 'u1', 'Alice', 'user', 'SYN-R-LOCAL 用户补充', true)`, rel)
	mustExec(t, db, `INSERT INTO lingo_ticket_comments (ticket_id, author_name, source, body, public, gitea_comment_id, model_marked) VALUES
			($1, 'dev', 'gitea', 'SYN-R-MARKED 对外回复', true, 201, true),
			($1, 'dev', 'gitea', 'SYN-R-PLAIN 普通评论', true, 202, false),
			($1, 'dev', 'gitea', 'SYN-R-LEGACY 旧评论', true, 203, NULL)`, rel)
	mustExec(t, db, `INSERT INTO lingo_knowledge (project_id, question, answer, source, source_ticket_id, status, search_tokens)
		SELECT id, '打印机卡纸', 'SYN-R-OLD 旧抽取', 'ticket_extract', t.tid, 'active', '{打印,卡纸}'
		FROM lingo_projects, (VALUES ($1::bigint), ($2::bigint)) t(tid) WHERE key = 'demo'`, rel, open)
	oldRow := func() string {
		var s string
		_ = db.QueryRow(`SELECT question || '|' || answer || '|' || status || '|' || COALESCE(model_ok::text, 'null') FROM lingo_knowledge
			WHERE source_ticket_id = $1 AND model_ok IS NULL`, rel).Scan(&s)
		return s
	}
	before := oldRow()

	// 权限：external / agent 403；越项目与不存在 404；先于查找
	scoped := lingo.User{ID: "r2", Name: "R2", Role: lingo.RoleReviewer, Projects: []string{"other"}}
	for _, c := range []struct {
		u    lingo.User
		id   int64
		want error
	}{{alice, rel, lingo.ErrForbidden}, {agent, rel, lingo.ErrForbidden}, {alice, 999999, lingo.ErrForbidden},
		{scoped, rel, lingo.ErrNotFound}, {rita, 999999, lingo.ErrNotFound},
		{rita, plain, lingo.ErrConflict}, {rita, open, lingo.ErrConflict}} {
		if _, err := l.ReextractTicketKnowledge(ctx, c.u, c.id); !errors.Is(err, c.want) {
			t.Errorf("%s %d: %v, want %v", c.u.ID, c.id, err, c.want)
		}
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_llm_usage WHERE kind = 'knowledge_extract'"); n != 0 {
		t.Errorf("model called on rejected requests: %d", n)
	}

	m.reset()
	r, err := l.ReextractTicketKnowledge(ctx, rita, rel)
	if err != nil || r.Outcome != lingo.KnowledgeReextractCreated || r.Entry == nil || r.Comments != 2 || r.Excluded != 2 {
		t.Fatalf("reextract = %+v %v", r, err)
	}
	if e := r.Entry; e.Status != lingo.KnowledgePending || !e.ModelInput || e.Source != lingo.KnowledgeSourceTicketExtract || e.SourceTicketID != rel {
		t.Errorf("new candidate = %+v", e)
	}
	contains(t, "reextract input", m.sent(), []string{"SYN-R-LOCAL", "SYN-R-MARKED"}, []string{"SYN-R-PLAIN", "SYN-R-LEGACY", "SYN-R-OLD"})
	if after := oldRow(); after != before || before == "" {
		t.Errorf("legacy entry changed: %q → %q", before, after)
	}
	// 重复调用：返回已有的可信候选，不再调用模型
	m.reset()
	if r2, err := l.ReextractTicketKnowledge(ctx, rita, rel); err != nil || r2.Outcome != lingo.KnowledgeReextractExists || r2.Entry == nil || r2.Entry.ID != r.Entry.ID {
		t.Errorf("repeat = %+v %v", r2, err)
	}
	if strings.TrimSpace(m.sent()) != "" {
		t.Errorf("repeat called model: %q", m.sent())
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_llm_usage WHERE kind = 'knowledge_extract'"); n != 1 {
		t.Errorf("extract usage = %d", n)
	}
	// 新候选走正常审核后才参与问答
	if _, err := l.ApproveKnowledge(ctx, rita, r.Entry.ID, nil); err != nil {
		t.Fatal(err)
	}
	c, _ := l.CreateConversation(ctx, alice, "")
	m.reset()
	m.setIntent(`{"intent":"knowledge_ask","keywords":["打印机","卡纸"]}`)
	send(t, l, alice, c.ID, "打印机卡纸怎么办")
	contains(t, "after approve", m.sent(), []string{"SYN-NEW-ANSWER"}, []string{"SYN-R-OLD"})

	// REST
	rc := restClient{t: t, h: l.Handler()}
	if code, obj, _ := rc.do("r1", "reviewer", "POST", fmt.Sprintf("/tickets/%d/knowledge/reextract", rel), nil); code != 200 || obj["outcome"] != "exists" {
		t.Errorf("rest reextract: %d %v", code, obj)
	}
	for _, c := range []struct {
		user, role string
		id         int64
		code       int
	}{{"u1", "external", rel, 403}, {"r1", "reviewer", 999999, 404}, {"r1", "reviewer", plain, 409}, {"", "", rel, 401}} {
		if code, obj, _ := rc.do(c.user, c.role, "POST", fmt.Sprintf("/tickets/%d/knowledge/reextract", c.id), nil); code != c.code {
			t.Errorf("rest %s %d: %d %v", c.role, c.id, code, obj)
		}
	}

	// 自动抽取（新结案工单）：只用范围内的评论，候选记为可信
	auto := seedIssueTicket(t, db, "demo", "api", "u1", "closed", 24)
	if _, err := db.Exec(`INSERT INTO lingo_ticket_comments (ticket_id, author_name, source, body, public, gitea_comment_id, model_marked) VALUES
		($1, 'dev', 'gitea', 'SYN-A-MARKED 对外回复', true, 301, true), ($1, 'dev', 'gitea', 'SYN-A-PLAIN 普通评论', true, 302, false)`, auto); err != nil {
		t.Fatal(err)
	}
	m.reset()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- l.Run(runCtx) }()
	mcWait(t, "auto extraction", func() bool {
		return countRows(t, db, "SELECT count(*) FROM lingo_knowledge_extractions WHERE ticket_id = $1 AND status = 'done'", auto) == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	contains(t, "auto extraction", m.sent(), []string{"SYN-A-MARKED"}, []string{"SYN-A-PLAIN"})
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source_ticket_id = $1 AND model_ok", auto); n != 1 {
		t.Errorf("auto candidate model_ok rows = %d", n)
	}

	// 没有抽取模型
	nl := newMCLingo(t, db, &commentGitea{comments: map[string][]map[string]any{}}, lingo.ReplyAll, lingo.ModelContentMarked, nil)
	if _, err := nl.ReextractTicketKnowledge(ctx, rita, open); !errors.Is(err, lingo.ErrConflict) {
		t.Errorf("open ticket without model: %v", err)
	}
	if _, err := db.Exec("UPDATE lingo_tickets SET status = 'released' WHERE id = $1", open); err != nil {
		t.Fatal(err)
	}
	if _, err := nl.ReextractTicketKnowledge(ctx, rita, open); !errors.Is(err, lingo.ErrNoModel) {
		t.Errorf("no model: %v", err)
	}
}

// 未启用新策略（默认）时既有行为不变：普通用户问答带全部对外评论、知识与历史不过滤、抽取用对外评论；只额外记录来源。
func TestModelContentDefaultUnchanged(t *testing.T) {
	db := testDB(t, true)
	g := &commentGitea{comments: map[string][]map[string]any{}}
	m := newRecModel()
	m.answer = func(data string) string {
		if strings.Contains(data, `"closed"`) {
			return extractJSON
		}
		return "好的"
	}
	l := newMCLingo(t, db, g, lingo.ReplyAll, lingo.ModelContentDefault, m)
	ctx := context.Background()
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	t1 := seedIssueTicket(t, db, "demo", "api", "u1", "in_progress", 12)
	g.set("demo/api#12", gcomment(101, "dev", "SYN-D-PLAIN 普通评论", at), gcomment(102, "dev", "/reply SYN-D-MARKED", at))
	if _, err := l.ResyncTicketComments(ctx, rita, t1, lingo.CommentResyncOptions{}); err != nil {
		t.Fatal(err)
	}
	s, msg := askTicket(t, l, m, alice, t1)
	contains(t, "default external", s, []string{"SYN-D-PLAIN", "SYN-D-MARKED"}, nil)
	if v := messageModelOK(t, db, msg.ID); !v.Valid || v.Bool {
		t.Errorf("default answer with unmarked comments model_ok = %+v", v)
	}
	// 来源未知的知识与历史照常交给模型
	if _, err := db.Exec(`INSERT INTO lingo_knowledge (project_id, question, answer, source, status, search_tokens)
		SELECT id, '打印机卡纸', 'SYN-D-LEGACY 旧候选', 'assistant_feedback', 'active', '{打印,印机,卡纸}' FROM lingo_projects WHERE key = 'demo'`); err != nil {
		t.Fatal(err)
	}
	c, _ := l.CreateConversation(ctx, alice, "")
	if _, err := db.Exec(`INSERT INTO lingo_assistant_messages (conversation_id, role, content, status)
		VALUES ($1, 'user', 'q0', 'done'), ($1, 'assistant', 'SYN-D-HIST 旧回答', 'done')`, c.ID); err != nil {
		t.Fatal(err)
	}
	m.reset()
	m.setIntent(`{"intent":"knowledge_ask","keywords":["打印机","卡纸"]}`)
	send(t, l, alice, c.ID, "打印机卡纸怎么办")
	contains(t, "default derived", m.sent(), []string{"SYN-D-LEGACY", "SYN-D-HIST"}, nil)
	// 自动抽取照常使用全部对外评论，候选如实记为不可信
	if _, err := db.Exec("UPDATE lingo_tickets SET status = 'released' WHERE id = $1", t1); err != nil {
		t.Fatal(err)
	}
	m.reset()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- l.Run(runCtx) }()
	mcWait(t, "extraction", func() bool {
		return countRows(t, db, "SELECT count(*) FROM lingo_knowledge_extractions WHERE ticket_id = $1 AND status = 'done'", t1) == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	contains(t, "default extraction", m.sent(), []string{"SYN-D-PLAIN", "SYN-D-MARKED"}, nil)
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE source_ticket_id = $1 AND model_ok = false", t1); n != 1 {
		t.Errorf("default candidate provenance rows = %d", n)
	}
	// 非法取值
	if _, err := lingo.New(lingo.Config{DB: db, Authenticate: headerUser, ModelContent: "all",
		Projects: []lingo.Project{{Key: "demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}}}); err == nil {
		t.Error("invalid ModelContent accepted")
	}
}

// 0009 增量升级：只加可空列、不改写已有行（来源一律为未知）；旧版本写入的抽取候选仍受“每张工单一条”约束。
func TestModelContentMigrationIncremental(t *testing.T) {
	db := testDB(t, true)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		DROP INDEX lingo_knowledge_extract_trusted_idx, lingo_knowledge_extract_ticket_idx;
		CREATE UNIQUE INDEX lingo_knowledge_extract_ticket_idx ON lingo_knowledge (source_ticket_id) WHERE source = 'ticket_extract';
		ALTER TABLE lingo_ticket_comments DROP COLUMN model_marked;
		ALTER TABLE lingo_assistant_messages DROP COLUMN model_ok;
		ALTER TABLE lingo_knowledge DROP COLUMN model_ok;
		DELETE FROM lingo_schema_migrations WHERE name LIKE '0009_%';
		INSERT INTO lingo_projects (key, name) VALUES ('demo', 'Demo');
		INSERT INTO lingo_tickets (user_id, user_name, user_email, project_id, type, title, body, status)
		SELECT 'u1', 'u1', '', id, 'bug', 'old', 'old', 'deployed' FROM lingo_projects WHERE key = 'demo';
		INSERT INTO lingo_ticket_comments (ticket_id, author_name, source, body, public, gitea_comment_id)
		SELECT id, 'dev', 'gitea', '/reply old', true, 9001 FROM lingo_tickets;
		INSERT INTO lingo_knowledge (project_id, question, answer, source, source_ticket_id, status)
		SELECT p.id, 'q', 'a', 'ticket_extract', t.id, 'active' FROM lingo_projects p, lingo_tickets t`); err != nil {
		t.Fatal(err)
	}
	if err := lingo.Migrate(ctx, db); err != nil {
		t.Fatalf("incremental Migrate: %v", err)
	}
	if st, err := lingo.CheckMigrations(ctx, db); err != nil || !st.Ready {
		t.Errorf("after incremental: %+v %v", st, err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_ticket_comments WHERE model_marked IS NULL"); n != 1 {
		t.Errorf("comment backfilled: %d", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge WHERE model_ok IS NULL"); n != 1 {
		t.Errorf("knowledge backfilled: %d", n)
	}
	// 旧版本（不写 model_ok）的第二条抽取候选仍冲突；可信候选可另存一条
	if _, err := db.ExecContext(ctx, `INSERT INTO lingo_knowledge (project_id, question, answer, source, source_ticket_id, status)
		SELECT p.id, 'q2', 'a2', 'ticket_extract', t.id, 'pending' FROM lingo_projects p, lingo_tickets t ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO lingo_knowledge (project_id, question, answer, source, source_ticket_id, status, model_ok)
		SELECT p.id, 'q3', 'a3', 'ticket_extract', t.id, 'pending', true FROM lingo_projects p, lingo_tickets t`); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM lingo_knowledge"); n != 2 {
		t.Errorf("extract rows = %d", n)
	}
}
