package ticket

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
)

type memAttachments struct{ saved []*Attachment }

func (m *memAttachments) SaveAttachment(_ context.Context, a *Attachment) error {
	a.ID = int64(len(m.saved) + 1)
	m.saved = append(m.saved, a)
	return nil
}
func (m *memAttachments) GetAttachment(context.Context, int64, bool) (*Attachment, error) {
	return nil, ErrNotFound
}
func (m *memAttachments) CommentPublic(context.Context, int64) (bool, error) { return true, nil }
func (m *memAttachments) DeleteOrphanAttachments(context.Context, time.Time) (int, error) {
	return 0, nil
}

var png1x1 = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestUploadValidation(t *testing.T) {
	ma := &memAttachments{}
	s, _ := NewService(ServiceOptions{Store: newMem(), Attachments: ma})
	ctx := context.Background()
	for name, c := range map[string]struct {
		file string
		data []byte
		ok   bool
	}{
		"png":        {"a.PNG", png1x1, true},
		"log":        {"app.log", []byte("x"), true},
		"exe":        {"a.exe", []byte("MZ"), false},
		"no ext":     {"README", []byte("x"), false},
		"fake image": {"a.jpg", []byte("text"), false},
		"empty":      {"a.txt", nil, false},
		"too big":    {"a.txt", []byte(strings.Repeat("x", 101)), false},
		"blank name": {"  ", []byte("x"), false},
		"path":       {"../../etc/x.txt", []byte("x"), true},
		"windows":    {"C:\\Users\\a\\b.txt", []byte("x"), true},
	} {
		a, err := s.Upload(ctx, alice, c.file, c.data, 100)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalid)) {
			t.Errorf("%s: %v", name, err)
		}
		if err == nil && strings.ContainsAny(a.Filename, "/\\") {
			t.Errorf("%s: filename %q", name, a.Filename)
		}
	}
	if a, _ := s.Upload(ctx, alice, "a.PNG", png1x1, 100); a.ContentType != "image/png" || a.UserID != "u1" || len(a.SHA256) != 32 || a.Data != nil {
		t.Errorf("attachment = %+v", a)
	}
	if _, err := s.Upload(ctx, auth0, "a.txt", []byte("x"), 100); !errors.Is(err, ErrForbidden) {
		t.Errorf("anonymous: %v", err)
	}
}

func TestAddComment(t *testing.T) {
	m := newMem()
	s := newService(t, m, map[string]bool{TypeBug: true})
	ctx := context.Background()
	pending, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeFeature, Body: "x"})
	fwd, _ := s.Create(ctx, alice, NewTicket{Project: "demo", Type: TypeBug, Body: "y"})
	m.tickets[fwd.ID].Status, m.tickets[fwd.ID].IssueNumber = StatusNeedInfo, 12
	m.uploads[7] = "u1"
	m.uploads[8] = "u2"

	if _, err := s.AddComment(ctx, alice, pending.ID, "补充", nil); err != nil {
		t.Fatal(err)
	}
	if len(m.tasks) != 1 { // 只有 bug 的建单任务；待确认工单的补充不发 Gitea
		t.Errorf("tasks = %v", m.tasks)
	}
	if _, err := s.AddComment(ctx, bob, fwd.ID, "x", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user: %v", err)
	}
	if _, err := s.AddComment(ctx, agent, fwd.ID, "x", nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent: %v", err)
	}
	if _, err := s.AddComment(ctx, alice, fwd.ID, "x", []int64{8}); !errors.Is(err, ErrInvalid) {
		t.Errorf("foreign attachment: %v", err)
	}
	if _, err := s.AddComment(ctx, alice, fwd.ID, " ", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty: %v", err)
	}
	c, err := s.AddComment(ctx, alice, fwd.ID, "已补充日志", []int64{7})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != "user" || !c.Public || m.tickets[fwd.ID].Status != StatusInProgress {
		t.Errorf("comment %+v status %s", c, m.tickets[fwd.ID].Status)
	}
	if _, ok := m.tasks["create_comment:comment:"+itoa(c.ID)]; !ok {
		t.Errorf("tasks = %v", m.tasks)
	}
	if rc, err := s.AddComment(ctx, reviewer, fwd.ID, "请重试", nil); err != nil || rc.Source != "staff" {
		t.Errorf("reviewer comment: %+v %v", rc, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

var auth0 = auth.Principal{}
