package tests

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zhouhao4221/lingo"
)

// scriptModel 按系统提示词区分三类调用：意图解析、确认判断、组织回答；依次返回预设输出，并记录收到的用户消息。
type scriptModel struct {
	mu        sync.Mutex
	intents   []string
	decisions []string
	answer    func(data string) string
	fail      int // 非 0 时接下来这么多次调用返回错误
	prompts   []string
	stream    bool
}

func (m *scriptModel) Chat(_ context.Context, ms []lingo.Message) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user := ms[len(ms)-1].Content
	m.prompts = append(m.prompts, user)
	if m.fail > 0 {
		m.fail--
		return "", errors.New("model down")
	}
	sys := ms[0].Content
	switch {
	case strings.Contains(sys, "意图解析器"):
		if len(m.intents) == 0 {
			return `{"intent":"clarify"}`, nil
		}
		out := m.intents[0]
		m.intents = m.intents[1:]
		return out, nil
	case strings.Contains(sys, "待确认操作的态度"):
		if len(m.decisions) == 0 {
			return `{"decision":"other"}`, nil
		}
		out := m.decisions[0]
		m.decisions = m.decisions[1:]
		return `{"decision":"` + out + `"}`, nil
	}
	if m.answer != nil {
		return m.answer(user), nil
	}
	return "好的", nil
}

func (m *scriptModel) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

func (m *scriptModel) allPrompts() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.prompts, "\n")
}

// streamModel 在 scriptModel 的基础上实现 lingo.StreamModel：回答按字拆成多个片段。
type streamModel struct{ *scriptModel }

func (m streamModel) ChatStream(ctx context.Context, ms []lingo.Message, onDelta func(string)) (string, lingo.Usage, error) {
	out, err := m.Chat(ctx, ms)
	if err != nil {
		return "", lingo.Usage{}, err
	}
	for _, r := range out {
		onDelta(string(r))
	}
	return out, lingo.Usage{Model: "main-1", Input: 10, Output: 5}, nil
}

func headerUser(r *http.Request) (lingo.User, error) {
	if r.Header.Get("X-User") == "" {
		return lingo.User{}, errors.New("anonymous")
	}
	return lingo.User{ID: r.Header.Get("X-User"), Name: r.Header.Get("X-User"), Role: lingo.Role(r.Header.Get("X-Role"))}, nil
}

func newAssistantLingo(t *testing.T, m lingo.Model) (*lingo.Lingo, *sql.DB) {
	t.Helper()
	return newAssistantLingoWith(t, m, &giteaServer{templates: "[]"})
}

func newAssistantLingoWith(t *testing.T, m lingo.Model, g *giteaServer) (*lingo.Lingo, *sql.DB) {
	t.Helper()
	db := testDB(t, true)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: m, SkipSelfCheck: true, ReconcileInterval: -1,
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
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
	return l, db
}

// seedTicketRow 直接写一张工单（repo 为 demo 下的仓库名）。
func seedTicketRow(t *testing.T, db *sql.DB, user, email, repo, typ, title, status string, issue int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO lingo_tickets (user_id, user_name, user_email, project_id, repo_id, type, title, body, status,
		gitea_issue_number, triage_since)
		SELECT $1, $1, $2, p.id, r.id, $4, $5, $5 || ' 的详细描述', $6, NULLIF($7, 0), CASE WHEN $6 = 'pending_triage' THEN now() END
		FROM lingo_projects p JOIN lingo_repos r ON r.project_id = p.id WHERE p.key = 'demo' AND r.name = $3 RETURNING id`,
		user, email, repo, typ, title, status, issue).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedReleaseRow(t *testing.T, db *sql.DB, tag string, tickets ...int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO lingo_releases (repo_id, tag, name, body, published_at, commit_sha)
		SELECT id, $1, $1, '修复登录问题', now() - interval '1 day', 'sha-' || $1 FROM lingo_repos WHERE owner = 'demo' AND name = 'api'
		RETURNING id`, tag).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, tk := range tickets {
		if _, err := db.Exec(`INSERT INTO lingo_release_tickets (release_id, ticket_id, ref) VALUES ($1, $2, 'commit')`, id, tk); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE lingo_tickets SET fixed_release_id = $1 WHERE id = $2`, id, tk); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

var (
	bob   = lingo.User{ID: "u2", Name: "Bob", Email: "bob@example.com", Role: lingo.RoleExternal}
	agent = lingo.User{ID: "a1", Name: "dev-agent", Role: lingo.RoleAgent}
	admin = lingo.User{ID: "root", Name: "Root", Role: lingo.RoleAdmin}
)

func ticketIDs(m lingo.ConversationMessage) []int64 {
	var ids []int64
	for _, c := range m.Citations.Tickets {
		ids = append(ids, c.ID)
	}
	return ids
}

func send(t *testing.T, l *lingo.Lingo, u lingo.User, conv int64, q string) lingo.ConversationMessage {
	t.Helper()
	m, err := l.SendMessage(context.Background(), u, conv, lingo.MessageInput{Content: q, TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("send %q: %v", q, err)
	}
	if m.Status != "done" {
		t.Fatalf("send %q: status %s (%s)", q, m.Status, m.Error)
	}
	return m
}

func TestAssistantTicketQuestionsRespectVisibility(t *testing.T) {
	m := &scriptModel{answer: func(data string) string {
		if strings.Contains(data, "ticket_detail") {
			var id int64
			_, _ = fmt.Sscanf(data[strings.Index(data, `"id":`)+5:], "%d", &id)
			return fmt.Sprintf("#%d 已修复，已在 v1.0.0 发布。", id)
		}
		return "共 2 张"
	}}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	mine := seedTicketRow(t, db, "u1", "alice@example.com", "api", "bug", "登录页白屏", "released", 12)
	theirs := seedTicketRow(t, db, "u2", "bob@example.com", "web", "bug", "登录超时", "pending_triage", 0)
	rel := seedReleaseRow(t, db, "v1.0.0", mine)

	conv, err := l.CreateConversation(ctx, alice, "")
	if err != nil {
		t.Fatal(err)
	}
	// external 只能找到自己的工单：唯一命中，模型依据数据作答，引用工单与修复版本
	m.intents = []string{`{"intent":"ticket_find","keywords":["登录"],"mine":true,"title":"登录 bug 进度"}`}
	got := send(t, l, alice, conv.ID, "我上周提的登录 bug 修了吗")
	if ids := ticketIDs(got); len(ids) != 1 || ids[0] != mine {
		t.Fatalf("citations = %v, want [%d]", ids, mine)
	}
	if len(got.Citations.Releases) != 1 || got.Citations.Releases[0].ID != rel || got.Citations.Releases[0].Tag != "v1.0.0" {
		t.Errorf("release citations = %+v", got.Citations.Releases)
	}
	if !strings.Contains(got.Content, fmt.Sprintf("#%d", mine)) {
		t.Errorf("content = %q", got.Content)
	}
	if got.Citations.Tickets[0].IssueURL == "" {
		t.Errorf("issue url missing: %+v", got.Citations.Tickets[0])
	}
	// 送给模型的数据不含提交人身份
	if p := m.allPrompts(); strings.Contains(p, "alice@example.com") || strings.Contains(p, "bob@example.com") {
		t.Errorf("prompt leaks email:\n%s", p)
	}
	// 他人的工单：按没找到回答
	m.intents = []string{fmt.Sprintf(`{"intent":"ticket_find","ticket_ids":[%d]}`, theirs)}
	if got := send(t, l, alice, conv.ID, fmt.Sprintf("#%d 怎么样了", theirs)); len(got.Citations.Tickets) != 0 ||
		!strings.Contains(got.Content, "没有找到") {
		t.Errorf("other's ticket: %+v", got)
	}
	// 会话标题取自首条提问的意图
	cs, err := l.ListConversations(ctx, alice, 0, 0)
	if err != nil || len(cs) != 1 || cs[0].Title != "登录 bug 进度" {
		t.Errorf("conversations = %+v %v", cs, err)
	}

	// reviewer 能看到两张：列出候选，不擅自挑一张
	rc, _ := l.CreateConversation(ctx, rita, "")
	m.intents = []string{`{"intent":"ticket_find","keywords":["登录"]}`}
	if got := send(t, l, rita, rc.ID, "登录相关的工单"); len(got.Citations.Tickets) != 2 || !strings.Contains(got.Content, "哪一张") {
		t.Errorf("candidates: %+v", got)
	}
	// agent 看不到未转出的工单
	ac, _ := l.CreateConversation(ctx, agent, "")
	m.intents = []string{fmt.Sprintf(`{"intent":"ticket_find","ticket_ids":[%d]}`, theirs)}
	if got := send(t, l, agent, ac.ID, fmt.Sprintf("#%d 的情况", theirs)); len(got.Citations.Tickets) != 0 {
		t.Errorf("agent sees pending ticket: %+v", got)
	}
	// 模型编造了数据之外的工单号：改用按数据生成的回答
	m.intents = []string{`{"intent":"ticket_find","keywords":["登录"],"mine":true}`}
	m.answer = func(string) string { return "#999 已修复" }
	if got := send(t, l, alice, conv.ID, "登录那个呢"); strings.Contains(got.Content, "#999") ||
		!strings.Contains(got.Content, fmt.Sprintf("#%d", mine)) {
		t.Errorf("fabricated answer kept: %q", got.Content)
	}
	// 超出范围
	m.intents = []string{`{"intent":"out_of_scope"}`}
	if got := send(t, l, alice, conv.ID, "这个功能怎么用"); !strings.Contains(got.Content, "只能回答工单和版本") {
		t.Errorf("out of scope: %q", got.Content)
	}
}

func TestAssistantSummaryAndReleases(t *testing.T) {
	m := &scriptModel{answer: func(data string) string { return "汇总如下" }}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	a := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "released", 12)
	b := seedTicketRow(t, db, "u2", "", "api", "bug", "支付失败", "released", 13)
	seedTicketRow(t, db, "u2", "", "web", "consult", "怎么导出", "pending_triage", 0)
	seedTicketRow(t, db, "u1", "", "web", "feature", "加个按钮", "pending_triage", 0)
	seedReleaseRow(t, db, "v1.0.0", a, b)

	// reviewer 问待确认数量：与 ListTriage 一致
	rc, _ := l.CreateConversation(ctx, rita, "")
	m.intents = []string{`{"intent":"ticket_summary","project":"demo","statuses":["pending_triage"]}`}
	got := send(t, l, rita, rc.ID, "demo 项目还有几个待确认的工单")
	triage, err := l.ListTriage(ctx, rita, lingo.TriageFilter{ProjectKey: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Citations.Tickets) != len(triage) || len(triage) != 2 {
		t.Errorf("summary cites %d, triage %d", len(got.Citations.Tickets), len(triage))
	}
	// 不在可访问列表中的项目：没找到
	m.intents = []string{`{"intent":"ticket_summary","project":"nope"}`}
	if got := send(t, l, rita, rc.ID, "nope 项目的工单"); !strings.Contains(got.Content, "没有找到") {
		t.Errorf("unknown project: %q", got.Content)
	}

	// external 问最近发版：版本修复的工单只含自己能看到的
	ec, _ := l.CreateConversation(ctx, alice, "")
	m.intents = []string{`{"intent":"release_list"}`}
	got = send(t, l, alice, ec.ID, "最近发版了什么")
	if len(got.Citations.Releases) != 1 || got.Citations.Releases[0].ProjectKey != "demo" {
		t.Fatalf("release cites = %+v", got.Citations.Releases)
	}
	if ids := ticketIDs(got); len(ids) != 1 || ids[0] != a {
		t.Errorf("release ticket cites = %v, want [%d]", ids, a)
	}
	m.mu.Lock()
	last := m.prompts[len(m.prompts)-1] // 组织回答的数据
	m.mu.Unlock()
	if !strings.Contains(last, "登录页白屏") || strings.Contains(last, "支付失败") {
		t.Errorf("release facts = %s", last)
	}
	// 用量记入三种用途
	var kinds int
	if err := db.QueryRow(`SELECT count(DISTINCT kind) FROM lingo_llm_usage WHERE kind LIKE 'assistant_%'`).Scan(&kinds); err != nil || kinds < 2 {
		t.Errorf("usage kinds = %d %v", kinds, err)
	}
}

func TestAssistantCommentNeedsConfirmation(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	id := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)
	conv, _ := l.CreateConversation(ctx, alice, "")
	q := fmt.Sprintf("帮我在 #%d 补一句：复现环境是 Chrome 120", id)
	intent := fmt.Sprintf(`{"intent":"comment","ticket_ids":[%d],"comment_body":"复现环境是 Chrome 120"}`, id)
	comments := func() int {
		cs, err := l.ListTicketComments(ctx, alice, id)
		if err != nil {
			t.Fatal(err)
		}
		return len(cs)
	}

	// 提出待确认操作，不写入
	m.intents = []string{intent}
	got := send(t, l, alice, conv.ID, q)
	if got.PendingAction == nil || got.PendingAction.Status != "pending" || comments() != 0 {
		t.Fatalf("propose: %+v, comments %d", got, comments())
	}
	// 回复“确认”：执行，同步 Issue
	m.decisions = []string{"confirm"}
	got = send(t, l, alice, conv.ID, "确认")
	if !strings.Contains(got.Content, "已在") || comments() != 1 {
		t.Fatalf("confirm: %q, comments %d", got.Content, comments())
	}
	var queued int
	if err := db.QueryRow(`SELECT count(*) FROM lingo_outbox WHERE ticket_id = $1 AND payload->>'op' = 'create_comment'`, id).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("create_comment tasks = %d", queued)
	}
	ms, err := l.ListConversationMessages(ctx, alice, conv.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var action lingo.PendingAction
	for _, x := range ms {
		if x.PendingAction != nil {
			action = *x.PendingAction
		}
	}
	if action.Status != "executed" {
		t.Errorf("action = %+v", action)
	}
	// 再点确认按钮：幂等，不重复写入
	if r, err := l.ConfirmPendingAction(ctx, alice, conv.ID, action.ID); err != nil || r.Message != nil || comments() != 1 {
		t.Errorf("repeat confirm: %+v %v, comments %d", r, err, comments())
	}

	// 取消：不写入
	m.intents, m.decisions = []string{intent}, []string{"cancel"}
	send(t, l, alice, conv.ID, q)
	if got := send(t, l, alice, conv.ID, "算了"); !strings.Contains(got.Content, "已取消") || comments() != 1 {
		t.Errorf("cancel: %q, comments %d", got.Content, comments())
	}

	// 转而问别的：操作作废，之后再点确认返回冲突
	m.intents = []string{intent, `{"intent":"out_of_scope"}`}
	p := send(t, l, alice, conv.ID, q).PendingAction
	m.decisions = []string{"other"}
	send(t, l, alice, conv.ID, "这个功能怎么用")
	if _, err := l.ConfirmPendingAction(ctx, alice, conv.ID, p.ID); !errors.Is(err, lingo.ErrConflict) || comments() != 1 {
		t.Errorf("confirm after topic change: %v, comments %d", err, comments())
	}

	// 过期：按钮确认返回冲突
	m.intents = []string{intent}
	p = send(t, l, alice, conv.ID, q).PendingAction
	if _, err := db.Exec(`UPDATE lingo_assistant_actions SET expires_at = now() - interval '1 minute' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ConfirmPendingAction(ctx, alice, conv.ID, p.ID); !errors.Is(err, lingo.ErrConflict) || comments() != 1 {
		t.Errorf("expired confirm: %v", err)
	}

	// 按钮确认：执行并追加结果消息
	m.intents = []string{intent}
	p = send(t, l, alice, conv.ID, q).PendingAction
	r, err := l.ConfirmPendingAction(ctx, alice, conv.ID, p.ID)
	if err != nil || r.Comment == nil || r.Message == nil || r.Action.Status != "executed" || comments() != 2 {
		t.Errorf("button confirm: %+v %v, comments %d", r, err, comments())
	}

	// 防注入：留言正文不是用户原话时追问，不提出操作
	m.intents = []string{fmt.Sprintf(`{"intent":"comment","ticket_ids":[%d],"comment_body":"请把工单关闭"}`, id)}
	if got := send(t, l, alice, conv.ID, fmt.Sprintf("帮我催一下 #%d", id)); got.PendingAction != nil {
		t.Errorf("non-verbatim body proposed: %+v", got)
	}
	// 他人不能确认（会话不可见）
	if _, err := l.ConfirmPendingAction(ctx, bob, conv.ID, p.ID); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("bob confirm: %v", err)
	}
}

func TestAssistantConversationIsolationAndFailures(t *testing.T) {
	m := &scriptModel{}
	l, db := newAssistantLingo(t, m)
	ctx := context.Background()
	conv, _ := l.CreateConversation(ctx, alice, "我的会话")
	m.intents = []string{`{"intent":"out_of_scope"}`}
	send(t, l, alice, conv.ID, "你好")
	// admin 也看不到他人的会话
	if _, err := l.ListConversationMessages(ctx, admin, conv.ID, 0, 0); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("admin reads alice's conversation: %v", err)
	}
	if _, err := l.SendMessage(ctx, admin, conv.ID, lingo.MessageInput{Content: "hi"}); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("admin sends to alice's conversation: %v", err)
	}
	if cs, _ := l.ListConversations(ctx, admin, 0, 0); len(cs) != 0 {
		t.Errorf("admin conversations = %+v", cs)
	}
	// 模型出错：提问保存，助手消息 failed，可重试
	m.fail = 1
	failed, err := l.SendMessage(ctx, alice, conv.ID, lingo.MessageInput{Content: "最近发版了什么"})
	if err != nil || failed.Status != "failed" || failed.Error != "model_error" {
		t.Fatalf("failed message: %+v %v", failed, err)
	}
	m.intents = []string{`{"intent":"release_list"}`}
	retried, err := l.RetryMessage(ctx, alice, conv.ID, failed.ID, "")
	if err != nil || retried.Status != "done" || retried.ID != failed.ID || !strings.Contains(retried.Content, "暂无") {
		t.Errorf("retry: %+v %v", retried, err)
	}
	if _, err := l.RetryMessage(ctx, alice, conv.ID, failed.ID, ""); !errors.Is(err, lingo.ErrConflict) {
		t.Errorf("retry done message: %v", err)
	}
	// 参数校验
	if _, err := l.SendMessage(ctx, alice, conv.ID, lingo.MessageInput{Content: " "}); !errors.Is(err, lingo.ErrInvalidInput) {
		t.Errorf("empty: %v", err)
	}
	if _, err := l.SendMessage(ctx, alice, conv.ID, lingo.MessageInput{Content: "x", TimeZone: "Mars/Base"}); !errors.Is(err, lingo.ErrInvalidInput) {
		t.Errorf("bad tz: %v", err)
	}
	// 删除后不可见，统计数据仍在
	if err := l.DeleteConversation(ctx, alice, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ListConversationMessages(ctx, alice, conv.ID, 0, 0); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("deleted conversation: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM lingo_assistant_messages WHERE conversation_id = $1`, conv.ID).Scan(&n); err != nil || n == 0 {
		t.Errorf("messages after delete = %d %v", n, err)
	}

	// 未配置模型：发送返回 ErrNoModel，会话接口可用
	nl, _ := newAssistantLingo(t, nil)
	c2, err := nl.CreateConversation(ctx, alice, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nl.SendMessage(ctx, alice, c2.ID, lingo.MessageInput{Content: "hi"}); !errors.Is(err, lingo.ErrNoModel) {
		t.Errorf("no model: %v", err)
	}
}

// sseEvents 解析 SSE 响应为（事件名, data）列表。
func sseEvents(body string) [][2]string {
	var out [][2]string
	var ev string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, [2]string{ev, strings.TrimPrefix(line, "data: ")})
		}
	}
	return out
}

func TestAssistantRESTStreamAndMCP(t *testing.T) {
	m := &scriptModel{answer: func(string) string { return "暂无相关信息" }}
	l, db := newAssistantLingo(t, streamModel{m})
	c := restClient{t: t, h: l.Handler()}
	id := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)

	code, conv, _ := c.do("u1", "external", "POST", "/conversations", nil)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, conv)
	}
	cid := int64(conv["id"].(float64))
	// 流式：若干 delta，最后恰好一个 message
	m.intents = []string{fmt.Sprintf(`{"intent":"ticket_find","ticket_ids":[%d]}`, id)}
	m.answer = func(string) string { return fmt.Sprintf("#%d 处理中", id) }
	body, _ := json.Marshal(map[string]any{"content": fmt.Sprintf("#%d 进展", id), "stream": true})
	r := httptest.NewRequest("POST", fmt.Sprintf("/conversations/%d/messages", cid), strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User", "u1")
	r.Header.Set("X-Role", "external")
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q: %s", ct, w.Body)
	}
	evs := sseEvents(w.Body.String())
	deltas, msgs := 0, 0
	var final map[string]any
	for _, e := range evs {
		switch e[0] {
		case "delta":
			deltas++
		case "message":
			msgs++
			_ = json.Unmarshal([]byte(e[1]), &final)
		}
	}
	if deltas < 2 || msgs != 1 || final["status"] != "done" || !strings.Contains(final["content"].(string), "处理中") {
		t.Errorf("events = %v", evs)
	}
	// 开始输出前的错误仍是 JSON：他人的会话 404
	if code, _, _ := c.do("u2", "external", "POST", fmt.Sprintf("/conversations/%d/messages", cid),
		map[string]any{"content": "hi", "stream": true}); code != http.StatusNotFound {
		t.Errorf("other's conversation: %d", code)
	}
	// 历史与列表
	if code, _, arr := c.do("u1", "external", "GET", fmt.Sprintf("/conversations/%d/messages", cid), nil); code != 200 || len(arr) != 2 {
		t.Errorf("messages: %d %v", code, arr)
	}
	// 未配置模型：501
	nl, _ := newAssistantLingo(t, nil)
	nc := restClient{t: t, h: nl.Handler()}
	_, c2, _ := nc.do("u1", "external", "POST", "/conversations", map[string]any{"title": "t"})
	if code, obj, _ := nc.do("u1", "external", "POST", fmt.Sprintf("/conversations/%d/messages", int64(c2["id"].(float64))),
		map[string]any{"content": "hi"}); code != http.StatusNotImplemented {
		t.Errorf("no model: %d %v", code, obj)
	}

	// MCP ask：新建会话并返回引用
	mc := mcpClient{t: t, h: l.MCPHandler()}
	m.intents = []string{fmt.Sprintf(`{"intent":"ticket_find","ticket_ids":[%d]}`, id)}
	res, ok := mc.call("u1", "external", "ask", map[string]any{"question": fmt.Sprintf("#%d 进展", id)})
	if !ok || res["conversation_id"] == nil {
		t.Fatalf("ask: %v", res)
	}
	msg := res["message"].(map[string]any)
	if cites := msg["citations"].(map[string]any)["tickets"].([]any); len(cites) != 1 {
		t.Errorf("ask cites: %v", msg)
	}
}
