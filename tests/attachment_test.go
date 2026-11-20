package tests

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo"
)

// pngData 是一张 1x1 的 PNG。
var pngData = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\xff\xff?\x00\x05\xfe\x02\xfe\xa7\x35\x81\x84\x00\x00\x00\x00IEND\xaeB\x60\x82")

func upload(t *testing.T, h http.Handler, user, role, name string, data []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/attachments", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("X-User", user)
	r.Header.Set("X-Role", role)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestAttachmentsAndCommentsFlow(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	db := testDB(t, true)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: nopModel{},
		Gitea: lingo.GiteaConfig{BaseURL: srv.URL, Token: "t", WebhookSecret: "s"},
		Projects: []lingo.Project{{Key: "demo", AutoForward: map[lingo.TicketType]bool{lingo.TicketBug: true},
			Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate: func(r *http.Request) (lingo.User, error) {
			if r.Header.Get("X-User") == "" {
				return lingo.User{}, errors.New("anonymous")
			}
			return lingo.User{ID: r.Header.Get("X-User"), Role: lingo.Role(r.Header.Get("X-Role"))}, nil
		},
		MaxAttachmentBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := restClient{t: t, h: l.Handler()}
	h := l.Handler()

	// 上传校验
	if code, body := upload(t, h, "u1", "external", "evil.exe", []byte("MZ")); code != 400 {
		t.Errorf("exe: %d %v", code, body)
	}
	if code, _ := upload(t, h, "u1", "external", "fake.png", []byte("not an image")); code != 400 {
		t.Errorf("fake png: %d", code)
	}
	if code, _ := upload(t, h, "u1", "external", "big.txt", bytes.Repeat([]byte("x"), 2048)); code != 400 {
		t.Errorf("too big: %d", code)
	}
	code, shot := upload(t, h, "u1", "external", "../../截图.png", pngData)
	if code != 201 || shot["filename"] != "截图.png" || shot["content_type"] != "image/png" {
		t.Fatalf("upload: %d %v", code, shot)
	}
	shotID := int64(shot["id"].(float64))
	_, logf := upload(t, h, "u1", "external", "app.log", []byte("panic: boom"))
	logID := int64(logf["id"].(float64))
	_, other := upload(t, h, "u2", "external", "x.txt", []byte("x"))
	otherID := int64(other["id"].(float64))

	// 别人的附件不能挂到自己的工单上
	if code, _, _ := c.do("u1", "external", "POST", "/tickets", map[string]any{"type": "bug", "content": "x", "attachment_ids": []int64{otherID}}); code != 400 {
		t.Errorf("foreign attachment: %d", code)
	}
	code, created, _ := c.do("u1", "external", "POST", "/tickets", map[string]any{"type": "bug", "content": "登录白屏", "attachment_ids": []int64{shotID}})
	if code != 201 {
		t.Fatalf("create: %d %v", code, created)
	}
	id := int64(created["id"].(float64))
	// 建单前补充（带附件）：写进 Issue 正文，附件随 Issue 上传，不单独发评论
	_, early := upload(t, h, "u1", "external", "trace.txt", []byte("stack"))
	earlyID := int64(early["id"].(float64))
	if code, _, _ := c.do("u1", "external", "POST", "/tickets/"+jsonNum(id)+"/comments", map[string]any{"body": "只在 Safari 出现", "attachment_ids": []int64{earlyID}}); code != 201 {
		t.Fatalf("comment before issue: %d", code)
	}
	if code, _, _ := c.do("u2", "external", "POST", "/tickets/"+jsonNum(id)+"/comments", map[string]any{"body": "x"}); code != 404 {
		t.Errorf("other user's comment: %d", code)
	}
	// 下载：本人可以，他人不行
	r := httptest.NewRequest("GET", "/attachments/"+jsonNum(shotID), nil)
	r.Header.Set("X-User", "u1")
	r.Header.Set("X-Role", "external")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pngData) || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("download: %d %v", w.Code, w.Header())
	}
	r.Header.Set("X-User", "u2")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("other user download: %d", w.Code)
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
	if !strings.Contains(edited, "![截图.png](http://g/attachments/1)") || !strings.Contains(edited, "[trace.txt](http://g/attachments/2)") ||
		!strings.HasSuffix(strings.TrimSpace(edited), "<!-- lingo:ticket="+jsonNum(id)+" -->") {
		t.Errorf("edited body:\n%s", edited)
	}

	// 建单后补充（带附件）：机器人发 Issue 评论并上传评论附件
	if code, _, _ := c.do("u1", "external", "POST", "/tickets/"+jsonNum(id)+"/comments", map[string]any{"body": "补充日志", "attachment_ids": []int64{logID}}); code != 201 {
		t.Fatalf("comment after issue: %d", code)
	}
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.edits) == 2
	})
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.comments) != 1 || !strings.Contains(g.comments[0], "来自提交人的补充") || !strings.Contains(g.comments[0], "补充日志") {
		t.Errorf("comments = %v", g.comments)
	}
	if len(g.assets) != 3 || !strings.HasPrefix(g.assets[2], "/repos/demo/api/issues/comments/501/assets:app.log") {
		t.Errorf("comment assets = %v", g.assets)
	}
	var forwarded int
	if err := db.QueryRow("SELECT count(*) FROM attachments WHERE forwarded").Scan(&forwarded); err != nil || forwarded != 3 {
		t.Errorf("forwarded = %d %v", forwarded, err)
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
