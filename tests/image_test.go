package tests

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zhouhao4221/lingo"
)

// 带图提问（REQ-009）。只使用程序生成的合成图片与脚本模型，不调用任何外部模型服务。

func synthPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 9))
	img.Set(1, 1, color.RGBA{G: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// visionModel 在 scriptModel 的基础上声明支持图片，并记录每次调用最后一条消息带的图片数。
type visionModel struct {
	*scriptModel
	mu     sync.Mutex
	images []int
	types  []string
}

func (m *visionModel) Chat(ctx context.Context, ms []lingo.Message) (string, error) {
	m.mu.Lock()
	last := ms[len(ms)-1]
	m.images = append(m.images, len(last.Images))
	for _, img := range last.Images {
		m.types = append(m.types, img.ContentType)
	}
	m.mu.Unlock()
	return m.scriptModel.Chat(ctx, ms)
}

func (m *visionModel) SupportsImages() bool { return true }

func (m *visionModel) imageCounts() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.images...)
}

// questionFiles 模拟接入方的附件系统：按上传人保存文件，FetchAttachment 只返回本人上传的。
type questionFiles struct {
	mu    sync.Mutex
	owner map[string]string // 附件 ID → 上传人
	data  map[string][]byte
	calls []lingo.Attachment
	users []string
}

func (f *questionFiles) fetch(_ context.Context, userID string, a lingo.Attachment) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.users = append(f.calls, a), append(f.users, userID)
	if f.owner[a.ID] != userID {
		return nil, lingo.ErrForbidden
	}
	return io.NopCloser(bytes.NewReader(f.data[a.ID])), nil
}

func newImageLingo(t *testing.T, m lingo.Model, fetch func(context.Context, string, lingo.Attachment) (io.ReadCloser, error)) (*lingo.Lingo, *sql.DB) {
	t.Helper()
	db := testDB(t, true)
	g := httptest.NewServer(&giteaServer{templates: "[]"})
	t.Cleanup(g.Close)
	l, err := lingo.New(lingo.Config{
		DB: db, Model: m, SkipSelfCheck: true, ReconcileInterval: -1,
		Gitea:           lingo.GiteaConfig{BaseURL: g.URL, Token: "t", WebhookSecret: "s"},
		Projects:        []lingo.Project{{Key: "demo", Name: "Demo", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},
		Authenticate:    headerUser,
		FetchAttachment: fetch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.ListTriage(context.Background(), rita, lingo.TriageFilter{}); err != nil {
		t.Fatal(err)
	}
	return l, db
}

func TestQuestionImagesAnswered(t *testing.T) {
	shot := synthPNG(t)
	files := &questionFiles{owner: map[string]string{"img-1": "u2"}, data: map[string][]byte{"img-1": shot}}
	m := &visionModel{scriptModel: &scriptModel{answer: func(string) string { return "截图里是导出报错，知识库暂无对应条目。" }}}
	l, _ := newImageLingo(t, m, files.fetch)
	ctx := context.Background()
	m.intents = []string{`{"intent":"out_of_scope","title":"导出报错截图"}`, `{"intent":"out_of_scope"}`}

	conv, err := l.CreateConversation(ctx, bob, "")
	if err != nil {
		t.Fatal(err)
	}
	att := lingo.Attachment{ID: "img-1", Name: "C:\\shots\\导出.png", ContentType: "image/png", Size: int64(len(shot))}
	got, err := l.SendMessage(ctx, bob, conv.ID, lingo.MessageInput{Attachments: []lingo.Attachment{att}}) // 只发图
	if err != nil || got.Status != "done" {
		t.Fatalf("send: %+v %v", got, err)
	}
	// 意图解析带上了图片（脚本模型同时充当主模型与轻量模型）
	if c := m.imageCounts(); len(c) != 1 || c[0] != 1 {
		t.Errorf("model image counts = %v", c)
	}
	files.mu.Lock()
	call, user := files.calls[0], files.users[0]
	files.mu.Unlock()
	if user != bob.ID || call.Purpose != lingo.AttachmentForQuestion || call.ConversationID != conv.ID || call.MessageID != got.ReplyTo ||
		call.TicketID != 0 || call.URL != "" || call.ContentType != "image/png" {
		t.Errorf("fetch = %q %+v", user, call)
	}
	ms, err := l.ListConversationMessages(ctx, bob, conv.ID, 0, 0)
	if err != nil || len(ms) != 2 {
		t.Fatalf("messages: %v %v", ms, err)
	}
	if a := ms[0].Attachments; ms[0].Role != "user" || len(a) != 1 || a[0].ID != "img-1" || a[0].Name != "导出.png" || a[0].URL != "" ||
		a[0].Size != int64(len(shot)) || ms[1].Attachments != nil {
		t.Errorf("echo = %+v / %+v", ms[0], ms[1])
	}
	cs, _ := l.ListConversations(ctx, bob, 0, 0)
	if len(cs) != 1 || cs[0].Title != "导出报错截图" {
		t.Errorf("title = %+v", cs)
	}
	// 下一条不带图：历史中只记图片张数，不再发图
	send(t, l, bob, conv.ID, "那怎么办")
	if c := m.imageCounts(); len(c) != 2 || c[1] != 0 {
		t.Errorf("history must not resend images: %v", c)
	}
	if p := m.allPrompts(); !strings.Contains(p, `"images": 1`) {
		t.Errorf("history should note the image count: %s", p)
	}
	// JSON 回显不含内部字段
	b, _ := json.Marshal(ms[0])
	if strings.Contains(string(b), "purpose") || strings.Contains(string(b), "message_id") || !strings.Contains(string(b), `"attachments"`) {
		t.Errorf("json = %s", b)
	}
}

func TestQuestionImagesAnswerSeesImages(t *testing.T) {
	shot := synthPNG(t)
	files := &questionFiles{owner: map[string]string{"img-1": "u2"}, data: map[string][]byte{"img-1": shot}}
	m := &visionModel{scriptModel: &scriptModel{answer: func(data string) string {
		i := strings.Index(data, `"id":`)
		id, _ := strconv.Atoi(strings.TrimRight(strings.SplitN(data[i+5:], ",", 2)[0], "}"))
		return "#" + strconv.Itoa(id) + " 正在处理"
	}}}
	l, db := newImageLingo(t, m, files.fetch)
	ctx := context.Background()
	var tid int64
	if err := db.QueryRow(`INSERT INTO lingo_tickets (user_id, user_name, project_id, repo_id, type, title, body, status)
		SELECT 'u2', 'Bob', p.id, r.id, 'bug', '导出报表为空', '导出为空', 'in_progress' FROM lingo_projects p JOIN lingo_repos r ON r.project_id = p.id
		RETURNING id`).Scan(&tid); err != nil {
		t.Fatal(err)
	}
	m.intents = []string{`{"intent":"ticket_find","ticket_ids":[` + strconv.FormatInt(tid, 10) + `]}`}
	conv, _ := l.CreateConversation(ctx, bob, "")
	got, err := l.SendMessage(ctx, bob, conv.ID, lingo.MessageInput{Content: "这个工单进展？",
		Attachments: []lingo.Attachment{{ID: "img-1", ContentType: "image/png"}}})
	if err != nil || got.Status != "done" || !strings.Contains(got.Content, "#"+strconv.FormatInt(tid, 10)) {
		t.Fatalf("send: %+v %v", got, err)
	}
	// 意图解析与组织回答都收到图片
	if c := m.imageCounts(); len(c) != 2 || c[0] != 1 || c[1] != 1 || m.types[1] != "image/png" {
		t.Errorf("image counts = %v %v", c, m.types)
	}
}

func TestQuestionImagesDeniedThenRetry(t *testing.T) {
	shot := synthPNG(t)
	files := &questionFiles{owner: map[string]string{"img-9": "someone-else"}, data: map[string][]byte{"img-9": shot}}
	m := &visionModel{scriptModel: &scriptModel{}}
	l, _ := newImageLingo(t, m, files.fetch)
	ctx := context.Background()
	conv, _ := l.CreateConversation(ctx, bob, "")
	in := lingo.MessageInput{Content: "看看这个", Attachments: []lingo.Attachment{{ID: "img-9", ContentType: "image/png"}}}
	got, err := l.SendMessage(ctx, bob, conv.ID, in)
	if err != nil || got.Status != "failed" || got.Error != "attachment_rejected" {
		t.Fatalf("denied: %+v %v", got, err)
	}
	if c := m.imageCounts(); len(c) != 0 {
		t.Errorf("model must not be called when an image is rejected: %v", c)
	}
	// 内容与声明不符同样拒绝
	files.mu.Lock()
	files.owner["img-9"], files.data["img-9"] = "u2", []byte("not a png")
	files.mu.Unlock()
	if got, err = l.RetryMessage(ctx, bob, conv.ID, got.ID, ""); err != nil || got.Error != "attachment_rejected" {
		t.Fatalf("bad content: %+v %v", got, err)
	}
	// 接入方修正后重试：复用原图片
	files.mu.Lock()
	files.data["img-9"] = shot
	files.mu.Unlock()
	m.intents = []string{`{"intent":"out_of_scope"}`}
	if got, err = l.RetryMessage(ctx, bob, conv.ID, got.ID, ""); err != nil || got.Status != "done" {
		t.Fatalf("retry: %+v %v", got, err)
	}
	files.mu.Lock()
	defer files.mu.Unlock()
	if len(files.calls) != 3 || files.calls[2].MessageID != files.calls[0].MessageID || files.calls[2].ID != "img-9" {
		t.Errorf("retry must refetch the original attachment: %+v", files.calls)
	}
	if c := m.imageCounts(); len(c) != 1 || c[0] != 1 {
		t.Errorf("model calls after retry = %v", c)
	}
}

func TestQuestionImagesUnsupportedAndInvalid(t *testing.T) {
	shot := synthPNG(t)
	files := &questionFiles{owner: map[string]string{"a": "u2"}, data: map[string][]byte{"a": shot}}
	img := []lingo.Attachment{{ID: "a", ContentType: "image/png"}}
	ctx := context.Background()

	// 模型没有声明支持图片：明确报错，提问不保存
	l, _ := newImageLingo(t, &scriptModel{}, files.fetch)
	conv, _ := l.CreateConversation(ctx, bob, "")
	if _, err := l.SendMessage(ctx, bob, conv.ID, lingo.MessageInput{Content: "q", Attachments: img}); !errors.Is(err, lingo.ErrImagesUnsupported) {
		t.Errorf("text model: %v", err)
	}
	if ms, _ := l.ListConversationMessages(ctx, bob, conv.ID, 0, 0); len(ms) != 0 {
		t.Errorf("rejected question must not be stored: %+v", ms)
	}
	// REST：501 images_unsupported
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/conversations/"+strconv.FormatInt(conv.ID, 10)+"/messages",
		strings.NewReader(`{"content":"q","attachments":[{"id":"a","content_type":"image/png"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User", "u2")
	req.Header.Set("X-Role", "external")
	l.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "images_unsupported") {
		t.Errorf("rest = %d %s", rec.Code, rec.Body)
	}

	// 未配置 FetchAttachment
	l2, _ := newImageLingo(t, &visionModel{scriptModel: &scriptModel{}}, nil)
	conv2, _ := l2.CreateConversation(ctx, bob, "")
	if _, err := l2.SendMessage(ctx, bob, conv2.ID, lingo.MessageInput{Attachments: img}); !errors.Is(err, lingo.ErrImagesUnsupported) {
		t.Errorf("no fetcher: %v", err)
	}

	// 参数校验
	l3, _ := newImageLingo(t, &visionModel{scriptModel: &scriptModel{}}, files.fetch)
	conv3, _ := l3.CreateConversation(ctx, bob, "")
	five := []lingo.Attachment{{ID: "1", ContentType: "image/png"}, {ID: "2", ContentType: "image/png"}, {ID: "3", ContentType: "image/png"},
		{ID: "4", ContentType: "image/png"}, {ID: "5", ContentType: "image/png"}}
	for name, in := range map[string]lingo.MessageInput{
		"url":       {Content: "q", Attachments: []lingo.Attachment{{ID: "a", ContentType: "image/png", URL: "https://files.example.com/a.png"}}},
		"gif":       {Content: "q", Attachments: []lingo.Attachment{{ID: "a", ContentType: "image/gif"}}},
		"no type":   {Content: "q", Attachments: []lingo.Attachment{{ID: "a"}}},
		"too many":  {Content: "q", Attachments: five},
		"too large": {Content: "q", Attachments: []lingo.Attachment{{ID: "a", ContentType: "image/png", Size: lingo.MaxQuestionImageBytes + 1}}},
		"empty":     {},
	} {
		if _, err := l3.SendMessage(ctx, bob, conv3.ID, in); !errors.Is(err, lingo.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// 带图的消息不会被当作对待确认操作的答复（不因图片旁的“好的”执行留言）。
func TestQuestionImagesDoNotConfirmActions(t *testing.T) {
	shot := synthPNG(t)
	files := &questionFiles{owner: map[string]string{"a": "u2"}, data: map[string][]byte{"a": shot}}
	m := &visionModel{scriptModel: &scriptModel{decisions: []string{"confirm"}}}
	l, db := newImageLingo(t, m, files.fetch)
	ctx := context.Background()
	var tid int64
	if err := db.QueryRow(`INSERT INTO lingo_tickets (user_id, user_name, project_id, repo_id, type, title, body, status)
		SELECT 'u2', 'Bob', p.id, r.id, 'bug', '登录失败', '登录失败', 'in_progress' FROM lingo_projects p JOIN lingo_repos r ON r.project_id = p.id
		RETURNING id`).Scan(&tid); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(tid, 10)
	m.intents = []string{`{"intent":"comment","ticket_ids":[` + id + `],"comment_body":"复现环境是 Chrome"}`, `{"intent":"confirm"}`,
		`{"intent":"comment","ticket_ids":[` + id + `],"comment_body":"截图"}`}
	conv, _ := l.CreateConversation(ctx, bob, "")
	p := send(t, l, bob, conv.ID, "在 #"+id+" 留言：复现环境是 Chrome")
	if p.PendingAction == nil {
		t.Fatalf("no pending action: %+v", p)
	}
	got, err := l.SendMessage(ctx, bob, conv.ID, lingo.MessageInput{Content: "好的", Attachments: []lingo.Attachment{{ID: "a", ContentType: "image/png"}}})
	if err != nil || got.Status != "done" {
		t.Fatalf("image reply: %+v %v", got, err)
	}
	var comments int
	if err := db.QueryRow("SELECT count(*) FROM lingo_ticket_comments WHERE ticket_id = $1", tid).Scan(&comments); err != nil || comments != 0 {
		t.Errorf("comment written by an image message: %d %v", comments, err)
	}
	if _, err := l.ConfirmPendingAction(ctx, bob, conv.ID, p.PendingAction.ID); !errors.Is(err, lingo.ErrConflict) {
		t.Errorf("action must be voided by the image message: %v", err)
	}
	// 带图时提出留言：说明不支持，不提出操作
	got, err = l.SendMessage(ctx, bob, conv.ID, lingo.MessageInput{Content: "在 #" + id + " 留言：截图", Attachments: []lingo.Attachment{{ID: "a", ContentType: "image/png"}}})
	if err != nil || got.PendingAction != nil || !strings.Contains(got.Content, "不支持附带图片") {
		t.Errorf("comment with image: %+v %v", got, err)
	}
}

// 数据库中的模型连接声明图片能力：按 OpenAI 兼容多模态格式发送（本地替身服务，合成图片）；取消声明后带图提问明确报错。
func TestQuestionImagesDatabaseSlot(t *testing.T) {
	shot := synthPNG(t)
	files := &questionFiles{owner: map[string]string{"a": "u2"}, data: map[string][]byte{"a": shot}}
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		out := "截图显示导出失败。"
		if strings.Contains(string(b), "意图解析器") {
			out = `{"intent":"out_of_scope"}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "vision-sim", "choices": []map[string]any{
			{"message": map[string]string{"content": out}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	l, _ := newImageLingo(t, nil, files.fetch)
	ctx := context.Background()
	openai, model := "openai", "vision-sim"
	s, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Model: &lingo.ModelConnectionUpdate{
		Main: &lingo.ModelSlotUpdate{Provider: &openai, BaseURL: sptr(srv.URL), Model: &model, Images: bptr(true)}}})
	if err != nil || !s.Model.Main.Images || s.Model.Light.Images {
		t.Fatalf("settings: %+v %v", s.Model, err)
	}
	conv, _ := l.CreateConversation(ctx, bob, "")
	img := []lingo.Attachment{{ID: "a", ContentType: "image/png"}}
	if got, err := l.SendMessage(ctx, bob, conv.ID, lingo.MessageInput{Content: "这是什么报错", Attachments: img}); err != nil || got.Status != "done" {
		t.Fatalf("send: %+v %v", got, err)
	}
	mu.Lock()
	n, joined := len(bodies), strings.Join(bodies, "\n")
	mu.Unlock()
	if n != 1 || !strings.Contains(joined, `"type":"image_url"`) || !strings.Contains(joined, "data:image/png;base64,") {
		t.Errorf("requests = %d: %.300s", n, joined)
	}

	// MCP ask 同样支持附带图片
	mc := mcpClient{t: t, h: l.MCPHandler()}
	if out, ok := mc.call("u2", "external", "ask", map[string]any{"attachments": []map[string]any{{"id": "a", "content_type": "image/png"}}}); !ok {
		t.Errorf("mcp ask with image: %v", out)
	}

	// 取消图片声明：带图提问明确报错，不发图片
	if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Model: &lingo.ModelConnectionUpdate{
		Main: &lingo.ModelSlotUpdate{Images: bptr(false)}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SendMessage(ctx, bob, conv.ID, lingo.MessageInput{Content: "再看看", Attachments: img}); !errors.Is(err, lingo.ErrImagesUnsupported) {
		t.Errorf("undeclared: %v", err)
	}
	if out, ok := mc.call("u2", "external", "ask", map[string]any{"question": "q", "attachments": []map[string]any{{"id": "a", "content_type": "image/png"}}}); ok ||
		!strings.Contains(out["error"].(string), "images") {
		t.Errorf("mcp ask undeclared: %v", out)
	}
	if cs, _ := l.ListConversations(ctx, bob, 0, 0); len(cs) != 2 {
		t.Errorf("a rejected MCP ask must not leave an empty conversation: %d", len(cs))
	}
	// DeepSeek 不能声明图片
	ds := "deepseek"
	if _, err := l.UpdateConnectionSettings(ctx, admin, lingo.ConnectionSettingsUpdate{Model: &lingo.ModelConnectionUpdate{
		Light: &lingo.ModelSlotUpdate{Provider: &ds, APIKey: sptr("k"), Images: bptr(true)}}}); !errors.Is(err, lingo.ErrInvalidInput) {
		t.Errorf("deepseek images: %v", err)
	}
}
