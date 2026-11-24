package tests

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zhouhao4221/lingo"
)

// audience 是测试用的 TicketAudience：按用户返回同事名单，记录调用的角色。
type audience struct {
	mu    sync.Mutex
	peers map[string][]string
	fail  bool
	calls []lingo.Role
}

func (a *audience) fn(_ context.Context, u lingo.User) (lingo.TicketAudience, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, u.Role)
	if a.fail {
		return lingo.TicketAudience{}, errors.New("directory down")
	}
	return lingo.TicketAudience{UserIDs: a.peers[u.ID]}, nil
}

func (a *audience) set(user string, peers ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.peers[user] = peers
}

func newAudienceLingo(t *testing.T, m lingo.Model) (*lingo.Lingo, *sql.DB, *audience) {
	t.Helper()
	db := testDB(t, true)
	srv := httptest.NewServer(&giteaServer{templates: "[]"})
	t.Cleanup(srv.Close)
	a := &audience{peers: map[string][]string{}}
	l, err := lingo.New(lingo.Config{
		DB: db, Model: m, SkipSelfCheck: true, ReconcileInterval: -1,
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		Projects: []lingo.Project{
			{Key: "demo", Name: "Demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}, {Owner: "demo", Name: "web"}}},
			{Key: "other", Name: "Other", Repos: []lingo.Repo{{Owner: "other", Name: "svc"}}}},
		Authenticate:   headerUser,
		TicketAudience: a.fn,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.ListTriage(context.Background(), rita, lingo.TriageFilter{}); err != nil { // 同步项目入库
		t.Fatal(err)
	}
	return l, db, a
}

// REQ-004：同事的工单只读可见——查看、列表、对外评论、版本；不能留言、补视角，看不到附件与事件。
func TestTicketAudienceReadOnly(t *testing.T) {
	l, db, a := newAudienceLingo(t, nopModel{})
	ctx := context.Background()
	bobT, err := l.CreateTicket(ctx, bob, lingo.TicketInput{Project: "demo", Type: lingo.TicketFeature, Title: "导出报表",
		Content: "希望支持导出", Attachments: []lingo.Attachment{{ID: "shot", Name: "s.png", URL: "https://app.example.com/f/s"}}})
	if err != nil {
		t.Fatal(err)
	}
	aliceT := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)
	daveT := seedTicketRow(t, db, "u4", "", "api", "bug", "主工单", "in_progress", 13)
	if _, err := db.Exec("UPDATE lingo_tickets SET status = 'merged', merged_into_ticket_id = $1 WHERE id = $2", daveT, bobT.ID); err != nil {
		t.Fatal(err)
	}

	// 未放宽：看不到
	if _, err := l.GetTicket(ctx, alice, bobT.ID); !errors.Is(err, lingo.ErrNotFound) {
		t.Fatalf("before audience: %v", err)
	}
	a.set("u1", "u2", "u1", "u2")
	got, err := l.GetTicket(ctx, alice, bobT.ID)
	if err != nil || got.Mine || got.UserName != "Bob" {
		t.Fatalf("peer ticket = %+v %v", got, err)
	}
	if own, err := l.GetTicket(ctx, alice, aliceT); err != nil || !own.Mine {
		t.Errorf("own = %+v %v", own, err)
	}
	// 合并工单按提交人判断：主工单（u4）不可见
	if _, err := l.GetTicket(ctx, alice, daveT); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("merge target visible: %v", err)
	}
	vs, err := l.ListVisibleTickets(ctx, alice, lingo.VisibleTicketFilter{})
	if err != nil || len(vs) != 2 || vs[0].ID != aliceT || vs[1].ID != bobT.ID || !vs[0].Mine || vs[1].Mine {
		t.Fatalf("visible = %+v %v", vs, err)
	}
	if vs, _ := l.ListVisibleTickets(ctx, alice, lingo.VisibleTicketFilter{Status: "merged"}); len(vs) != 1 || vs[0].ID != bobT.ID {
		t.Errorf("status filter = %+v", vs)
	}
	if vs, _ := l.ListVisibleTickets(ctx, alice, lingo.VisibleTicketFilter{Project: "other"}); len(vs) != 0 {
		t.Errorf("project filter = %+v", vs)
	}
	if ms, _ := l.ListMyTickets(ctx, alice, "", 0, 0); len(ms) != 1 || ms[0].ID != aliceT {
		t.Errorf("mine = %+v", ms)
	}
	if _, err := l.ListTicketComments(ctx, alice, bobT.ID); err != nil {
		t.Errorf("comments: %v", err)
	}
	// 写操作与视角：not_found
	if _, err := l.AddTicketComment(ctx, alice, bobT.ID, "我也遇到了", nil); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("peer comment: %v", err)
	}
	if _, err := l.ListPerspectives(ctx, alice, bobT.ID); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("peer perspectives: %v", err)
	}
	// 事件流不放宽
	evs, err := l.ListEvents(ctx, alice, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.TicketID == bobT.ID {
			t.Errorf("peer event leaked: %+v", e)
		}
	}
	// 不超出 User.Projects
	scoped := alice
	scoped.Projects = []string{"other"}
	if _, err := l.GetTicket(ctx, scoped, bobT.ID); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("outside projects: %v", err)
	}
	// REST：详情不含附件（提交人本人能看到）；/tickets/visible 与 Go 方法一致
	rc := restClient{t: t, h: l.Handler()}
	if code, obj, _ := rc.do("u1", "external", "GET", fmt.Sprintf("/tickets/%d", bobT.ID), nil); code != 200 ||
		len(obj["attachments"].([]any)) != 0 || obj["mine"] != false {
		t.Errorf("rest detail for peer: %d %v", code, obj)
	}
	if _, obj, _ := rc.do("u2", "external", "GET", fmt.Sprintf("/tickets/%d", bobT.ID), nil); len(obj["attachments"].([]any)) != 1 {
		t.Errorf("rest detail for submitter: %v", obj)
	}
	if code, _, arr := rc.do("u1", "external", "GET", "/tickets/visible", nil); code != 200 || len(arr) != 2 {
		t.Errorf("rest visible: %d %v", code, arr)
	}
	mc := mcpClient{t: t, h: l.MCPHandler()}
	if out, ok := mc.call("u1", "external", "list_visible_tickets", map[string]any{}); !ok {
		t.Errorf("mcp: %v", out)
	}
	// 版本关联的工单：同事的也列出
	rel := seedReleaseRow(t, db, "v1.0.0", bobT.ID)
	if d, err := l.GetRelease(ctx, alice, rel); err != nil || len(d.Tickets) != 1 || d.Tickets[0].Mine {
		t.Errorf("release = %+v %v", d.Tickets, err)
	}

	// 只对 external 调用
	a.mu.Lock()
	a.calls = nil
	a.mu.Unlock()
	if _, err := l.GetTicket(ctx, rita, bobT.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ListVisibleTickets(ctx, lingo.User{Role: lingo.RoleExternal}, lingo.VisibleTicketFilter{}); !errors.Is(err, lingo.ErrForbidden) {
		t.Errorf("no id: %v", err)
	}
	a.mu.Lock()
	if len(a.calls) != 0 {
		t.Errorf("callback called for %v", a.calls)
	}
	a.mu.Unlock()
	// 回调出错：按未放宽处理，请求不失败
	a.mu.Lock()
	a.fail = true
	a.mu.Unlock()
	if vs, err := l.ListVisibleTickets(ctx, alice, lingo.VisibleTicketFilter{}); err != nil || len(vs) != 1 || vs[0].ID != aliceT {
		t.Errorf("callback error: %+v %v", vs, err)
	}
	if _, err := l.GetTicket(ctx, alice, bobT.ID); !errors.Is(err, lingo.ErrNotFound) {
		t.Errorf("callback error get: %v", err)
	}
}

// 问答：汇总包含同事的工单（带提交人姓名），不能对同事的工单留言；同事不在名单后，历史引用只保留编号。
func TestTicketAudienceAssistant(t *testing.T) {
	m := &scriptModel{answer: func(string) string { return "汇总如下" }}
	l, db, a := newAudienceLingo(t, m)
	ctx := context.Background()
	mine := seedTicketRow(t, db, "u1", "", "api", "bug", "登录页白屏", "in_progress", 12)
	theirs := seedTicketRow(t, db, "u2", "", "web", "bug", "登录超时", "fixed", 13)
	a.set("u1", "u2")
	conv, _ := l.CreateConversation(ctx, alice, "")

	m.intents = []string{`{"intent":"ticket_summary","keywords":["登录"]}`}
	got := send(t, l, alice, conv.ID, "我们组最近提的登录问题修了吗")
	if ids := ticketIDs(got); len(ids) != 2 {
		t.Fatalf("citations = %v", ids)
	}
	if p := m.allPrompts(); !strings.Contains(p, `"submitter":"u2"`) || !strings.Contains(p, `"mine":true`) {
		t.Errorf("facts lack mine / submitter:\n%s", p)
	}
	// "我的"只含本人
	m.intents = []string{`{"intent":"ticket_summary","keywords":["登录"],"mine":true}`}
	if got := send(t, l, alice, conv.ID, "我提的登录问题"); len(ticketIDs(got)) != 1 || ticketIDs(got)[0] != mine {
		t.Errorf("mine citations = %v", ticketIDs(got))
	}
	// 对同事的工单留言：只读提示，不提出待确认操作
	m.intents = []string{fmt.Sprintf(`{"intent":"comment","ticket_ids":[%d],"comment_body":"我也遇到了"}`, theirs)}
	if got := send(t, l, alice, conv.ID, fmt.Sprintf("在 #%d 留言：我也遇到了", theirs)); got.PendingAction != nil ||
		!strings.Contains(got.Content, "只能对自己提交的工单留言") {
		t.Errorf("peer comment: %+v", got)
	}
	// 同事不在名单后：历史引用按当前范围重新判断
	a.set("u1")
	ms, err := l.ListConversationMessages(ctx, alice, conv.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawMine, sawTheirs bool
	for _, msg := range ms {
		for _, c := range msg.Citations.Tickets {
			switch c.ID {
			case theirs:
				sawTheirs = true
				if c.Title != "" || c.Status != "" {
					t.Errorf("stale citation kept: %+v", c)
				}
			case mine:
				sawMine = true
				if c.Title == "" {
					t.Errorf("own citation cleared: %+v", c)
				}
			}
		}
	}
	if !sawMine || !sawTheirs {
		t.Errorf("citations missing: mine=%v theirs=%v", sawMine, sawTheirs)
	}
}
