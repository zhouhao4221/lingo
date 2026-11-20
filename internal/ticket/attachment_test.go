package ticket

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestCleanAttachments(t *testing.T) {
	for name, c := range map[string]struct {
		in []Attachment
		ok bool
	}{
		"ok":         {[]Attachment{{ID: "f1", Name: "a.png", URL: "https://app.example.com/files/f1", ContentType: "image/png", Size: 3}}, true},
		"no url":     {[]Attachment{{ID: "f1", Name: "a.png"}}, true},
		"no id":      {[]Attachment{{Name: "a.png"}}, false},
		"dup id":     {[]Attachment{{ID: "f1"}, {ID: "f1"}}, false},
		"relative":   {[]Attachment{{ID: "f1", URL: "/files/f1"}}, false},
		"javascript": {[]Attachment{{ID: "f1", URL: "javascript:alert(1)"}}, false},
		"markdown":   {[]Attachment{{ID: "f1", URL: "https://x/a>)[y](z"}}, false},
		"space":      {[]Attachment{{ID: "f1", URL: "https://x/a b"}}, false},
		"negative":   {[]Attachment{{ID: "f1", Size: -1}}, false},
		"control id": {[]Attachment{{ID: "f\n1"}}, false},
		"too many":   {make([]Attachment, MaxAttachmentsPerTicket+1), false},
	} {
		if c.in != nil && name == "too many" {
			for i := range c.in {
				c.in[i].ID = strconv.Itoa(i)
			}
		}
		out, err := CleanAttachments(c.in)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalid)) {
			t.Errorf("%s: %v", name, err)
		}
		_ = out
	}
	out, _ := CleanAttachments([]Attachment{{ID: " f1 ", Name: "../../etc/x.txt", ContentType: "bad type///", State: AttachmentUploaded,
		GiteaURL: "https://gitea/x"}, {ID: "C:\\dir\\b.log"}})
	if a := out[0]; a.ID != "f1" || a.Name != "x.txt" || a.ContentType != "" || a.Forwarded() || a.GiteaURL != "" {
		t.Errorf("cleaned = %+v", a)
	}
	if out[1].Name != "b.log" { // 未给文件名：取 ID 的文件名部分
		t.Errorf("name from id = %q", out[1].Name)
	}
}

func TestCleanFilenameLongExtension(t *testing.T) {
	name := cleanFilename("a." + strings.Repeat("x", 250))
	if len([]rune(name)) != 200 {
		t.Errorf("len = %d", len([]rune(name)))
	}
	if got := cleanFilename(strings.Repeat("长", 300) + ".png"); len([]rune(got)) != 200 || !strings.HasSuffix(got, ".png") {
		t.Errorf("long name = %q", got)
	}
}

func TestForwardSelection(t *testing.T) {
	m := newMem()
	s := newService(t, m, nil)
	ctx := context.Background()
	tk, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "x", Attachments: []Attachment{{ID: "a"}, {ID: "b"}}})
	// 转出前的补充：附件 ID 在整个工单内唯一，补充评论的附件同样受勾选限制
	if _, err := s.AddComment(ctx, alice, tk.ID, "再补一张", []Attachment{{ID: "a"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("duplicate id across ticket: %v", err)
	}
	if _, err := s.AddComment(ctx, alice, tk.ID, "再补一张", []Attachment{{ID: "c"}}); err != nil {
		t.Fatal(err)
	}
	pick := []string{"a"}
	if _, err := s.Forward(ctx, reviewer, tk.ID, ForwardInput{Attachments: &pick}); err != nil {
		t.Fatal(err)
	}
	for _, task := range m.tasks {
		if ex, ok := task.Payload["exclude_attachment_ids"].([]string); !ok || len(ex) != 2 || ex[0] != "b" || ex[1] != "c" {
			t.Errorf("exclude = %#v", task.Payload["exclude_attachment_ids"])
		}
	}
	// 未勾选（nil）：全部转出，不记排除
	tk2, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "y", Attachments: []Attachment{{ID: "a"}}})
	if _, err := s.Forward(ctx, reviewer, tk2.ID, ForwardInput{}); err != nil {
		t.Fatal(err)
	}
	for k, task := range m.tasks {
		if task.TicketID == tk2.ID && task.Payload["exclude_attachment_ids"] != nil {
			t.Errorf("%s: %#v", k, task.Payload)
		}
	}
}

func TestAddComment(t *testing.T) {
	m := newMem()
	s := newService(t, m, map[string]bool{TypeBug: true})
	ctx := context.Background()
	pending, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeFeature, Body: "x"})
	fwd, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "y",
		Attachments: []Attachment{{ID: "a1"}, {ID: "a2"}, {ID: "a3"}, {ID: "a4"}, {ID: "a5"}, {ID: "a6"}, {ID: "a7"}, {ID: "a8"}, {ID: "a9"}}})
	m.tickets[fwd.ID].Status, m.tickets[fwd.ID].IssueNumber = StatusNeedInfo, 12
	log := []Attachment{{ID: "log", Name: "app.log", URL: "https://app.example.com/f/log"}}

	if _, err := s.AddComment(ctx, alice, pending.ID, "补充", nil); err != nil {
		t.Fatal(err)
	}
	if len(m.tasks) != 1 { // 只有 bug 的建单任务；待确认工单的补充不发 Gitea
		t.Errorf("tasks = %v", m.tasks)
	}
	if _, err := s.AddComment(ctx, bob, fwd.ID, "x", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user: %v", err)
	}
	if ac, err := s.AddComment(ctx, agent, fwd.ID, "正在排查", nil); err != nil || ac.Source != "agent" {
		t.Errorf("agent reply: %+v %v", ac, err)
	}
	if _, err := s.AddComment(ctx, agent, pending.ID, "x", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("agent on pending ticket: %v", err)
	}
	if _, err := s.AddComment(ctx, agent, fwd.ID, "x", log); !errors.Is(err, ErrInvalid) {
		t.Errorf("agent attachment: %v", err)
	}
	if _, err := s.AddComment(ctx, alice, fwd.ID, "x", []Attachment{{ID: "x", URL: "ftp://a/b"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad url: %v", err)
	}
	if _, err := s.AddComment(ctx, alice, fwd.ID, " ", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty: %v", err)
	}
	c, err := s.AddComment(ctx, alice, fwd.ID, "已补充日志", log)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != "user" || !c.Public || m.tickets[fwd.ID].Status != StatusInProgress || len(c.Attachments) != 1 {
		t.Errorf("comment %+v status %s", c, m.tickets[fwd.ID].Status)
	}
	if _, ok := m.tasks["create_comment:comment:"+itoa(c.ID)]; !ok {
		t.Errorf("tasks = %v", m.tasks)
	}
	// 工单含补充已有 10 个附件：再加超出上限
	if _, err := s.AddComment(ctx, alice, fwd.ID, "再补一张", []Attachment{{ID: "more"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("over limit: %v", err)
	}
	if rc, err := s.AddComment(ctx, reviewer, fwd.ID, "请重试", nil); err != nil || rc.Source != "staff" {
		t.Errorf("reviewer comment: %+v %v", rc, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
