package tests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// pngData 是一张 1x1 的 PNG。
var pngData = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\xff\xff?\x00\x05\xfe\x02\xfe\xa7\x35\x81\x84\x00\x00\x00\x00IEND\xaeB\x60\x82")

func headerAuth(r *http.Request) (lingo.User, error) {
	if r.Header.Get("X-User") == "" {
		return lingo.User{}, errors.New("anonymous")
	}
	return lingo.User{ID: r.Header.Get("X-User"), Role: lingo.Role(r.Header.Get("X-Role"))}, nil
}

func ref(id, name, ctype string) map[string]any {
	return map[string]any{"id": id, "name": name, "url": "https://app.example.com/files/" + id, "content_type": ctype}
}

// 附件由接入方保存，灵工只记引用；配置 FetchAttachment 时读取内容上传为 Gitea 附件，读取失败的退回为链接。
func TestAttachmentsAndCommentsFlow(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	db := testDB(t, true)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	files := map[string][]byte{"shot": pngData, "trace": []byte("stack"), "log": []byte("panic: boom")}
	var fetchedBy sync.Map // 附件 ID → FetchAttachment 收到的提交人
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{},
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		Projects: []lingo.Project{{Key: "demo", AutoForward: map[lingo.TicketType]bool{lingo.TicketBug: true},
			Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate: headerAuth,
		FetchAttachment: func(_ context.Context, userID string, a lingo.Attachment) (io.ReadCloser, error) {
			fetchedBy.Store(a.ID, userID)
			b, ok := files[a.ID]
			if !ok { // 接入方校验归属：不是该用户的附件
				return nil, fmt.Errorf("file %s not owned by %s: %w", a.ID, userID, lingo.ErrNotFound)
			}
			return io.NopCloser(bytes.NewReader(b)), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := restClient{t: t, h: l.Handler()}

	// 引用校验：地址须为 http(s) 绝对地址
	if code, _, _ := c.do("u1", "external", "POST", "/tickets", map[string]any{"type": "bug", "content": "x",
		"attachments": []any{map[string]any{"id": "a", "url": "javascript:alert(1)"}}}); code != 400 {
		t.Errorf("bad url: %d", code)
	}
	code, created, _ := c.do("u1", "external", "POST", "/tickets", map[string]any{"type": "bug", "content": "登录白屏",
		"attachments": []any{ref("shot", "../../截图.png", "image/png"), ref("gone", "gone.txt", "text/plain")}})
	if code != 201 {
		t.Fatalf("create: %d %v", code, created)
	}
	id := int64(created["id"].(float64))
	// 建单前补充（带附件）：写进 Issue 正文，附件随 Issue 上传，不单独发评论
	if code, _, _ := c.do("u1", "external", "POST", "/tickets/"+jsonNum(id)+"/comments", map[string]any{"body": "只在 Safari 出现",
		"attachments": []any{ref("trace", "trace.txt", "text/plain")}}); code != 201 {
		t.Fatalf("comment before issue: %d", code)
	}
	if code, _, _ := c.do("u2", "external", "POST", "/tickets/"+jsonNum(id)+"/comments", map[string]any{"body": "x"}); code != 404 {
		t.Errorf("other user's comment: %d", code)
	}

	runLingo(t, l)
	alice := lingo.User{ID: "u1", Role: lingo.RoleExternal}
	waitTicket(t, l, alice, id, "open")
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.edits) == 1
	})
	g.mu.Lock()
	body, _ := g.created[0]["body"].(string)
	edited := g.edits[0]
	assets := append([]string(nil), g.assets...)
	g.mu.Unlock()
	if !strings.Contains(body, "只在 Safari 出现") || len(assets) != 2 || !strings.HasPrefix(assets[0], "/repos/demo/api/issues/40/assets:截图.png") ||
		!strings.HasPrefix(assets[1], "/repos/demo/api/issues/40/assets:trace.txt") {
		t.Errorf("issue body / assets: %s %v", body, assets)
	}
	if !strings.Contains(edited, "![截图.png](<http://g/attachments/1>)") || !strings.Contains(edited, "[trace.txt](<http://g/attachments/2>)") ||
		!strings.Contains(edited, "[gone.txt](<https://app.example.com/files/gone>)（未能上传到 Gitea）") ||
		!strings.HasSuffix(strings.TrimSpace(edited), "<!-- lingo:ticket="+jsonNum(id)+" -->") {
		t.Errorf("edited body:\n%s", edited)
	}

	// 建单后补充（带附件）：机器人发 Issue 评论并上传评论附件
	if code, _, _ := c.do("u1", "external", "POST", "/tickets/"+jsonNum(id)+"/comments", map[string]any{"body": "补充日志",
		"attachments": []any{ref("log", "app.log", "text/plain")}}); code != 201 {
		t.Fatalf("comment after issue: %d", code)
	}
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.edits) == 2
	})
	g.mu.Lock()
	if len(g.comments) != 1 || !strings.Contains(g.comments[0], "补充（提交人）") || !strings.Contains(g.comments[0], "补充日志") {
		t.Errorf("comments = %v", g.comments)
	}
	if len(g.assets) != 3 || !strings.HasPrefix(g.assets[2], "/repos/demo/api/issues/comments/501/assets:app.log") {
		t.Errorf("comment assets = %v", g.assets)
	}
	g.mu.Unlock()

	// 详情：附件引用与转出结果
	_, detail, _ := c.do("u1", "external", "GET", "/tickets/"+jsonNum(id), nil)
	as, _ := detail["attachments"].([]any)
	if len(as) != 4 {
		t.Fatalf("attachments = %v", detail["attachments"])
	}
	first := as[0].(map[string]any)
	if first["id"] != "shot" || first["name"] != "截图.png" || first["forwarded"] != true {
		t.Errorf("first = %v", first)
	}
	if gone := as[1].(map[string]any); gone["forwarded"] != true || gone["error"] != nil || gone["gitea_url"] != nil {
		t.Errorf("gone (external) = %v", gone) // 失败原因与 Gitea 地址是内部信息
	}
	_, staff, _ := c.do("r1", "reviewer", "GET", "/tickets/"+jsonNum(id), nil)
	if gone := staff["attachments"].([]any)[1].(map[string]any); !strings.Contains(gone["error"].(string), "not owned by u1") {
		t.Errorf("gone (reviewer) = %v", gone)
	}
	fetchedBy.Range(func(k, v any) bool {
		if v != "u1" {
			t.Errorf("FetchAttachment(%v) user = %v", k, v)
		}
		return true
	})
	var uploaded int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM lingo_tickets t, jsonb_array_elements(t.attachments) a WHERE a->>'state' = 'uploaded')
		+ (SELECT count(*) FROM lingo_ticket_comments c, jsonb_array_elements(c.attachments) a WHERE a->>'state' = 'uploaded')`).Scan(&uploaded); err != nil || uploaded != 3 {
		t.Errorf("uploaded = %d %v", uploaded, err)
	}
}

// 未配置 FetchAttachment：只把附件地址写进 Issue，不上传。
func TestAttachmentLinksWithoutFetch(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	db := testDB(t, true)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{},
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		Projects: []lingo.Project{{Key: "demo", AutoForward: map[lingo.TicketType]bool{lingo.TicketBug: true},
			Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate: headerAuth,
	})
	if err != nil {
		t.Fatal(err)
	}
	alice := lingo.User{ID: "u1", Role: lingo.RoleExternal}
	tk, err := l.CreateTicket(context.Background(), alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "导出失败",
		Attachments: []lingo.Attachment{{ID: "a", Name: "a.png", URL: "https://app.example.com/files/a", ContentType: "image/png"}, {ID: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	waitTicket(t, l, alice, tk.ID, "open")
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.edits) == 1
	})
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.assets) != 0 || !strings.Contains(g.edits[0], "- [a.png](<https://app.example.com/files/a>)") ||
		!strings.Contains(g.edits[0], "- b（请在业务系统中查看）") {
		t.Errorf("assets %v body:\n%s", g.assets, g.edits[0])
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
