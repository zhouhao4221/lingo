package ticket

import (
	"context"
	"errors"
	"testing"

	"github.com/zhouhao4221/lingo/internal/auth"
)

// ResyncComment：只写入指定工单；Silent 不发事件但照常保存与判定；重复执行为 Unchanged。
func TestResyncCommentGuardAndSilent(t *testing.T) {
	m := newMem(Ticket{ID: 1, ProjectKey: "demo", Repo: "demo/api", IssueNumber: 12, Status: StatusOpen, UserID: "u1"})
	sy, err := NewSync(SyncOptions{Store: m})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u := CommentUpdate{Change: CommentEdited, Repo: "demo/api", Number: 12, CommentID: 5, Author: "dev", Body: "已修复", Public: true,
		At: t0, TicketID: 2, Silent: true}
	if r, err := sy.ResyncComment(ctx, u); err != nil || r.Outcome != CommentSkipped || len(m.comments) != 0 {
		t.Fatalf("other ticket: %+v %v %d", r, err, len(m.comments))
	}
	u.TicketID = 1
	if r, err := sy.ResyncComment(ctx, u); err != nil || r.Outcome != CommentAdded || !r.Published || len(m.events) != 0 {
		t.Fatalf("silent add: %+v %v events %d", r, err, len(m.events))
	}
	if r, err := sy.ResyncComment(ctx, u); err != nil || r.Outcome != CommentUnchanged || r.Published {
		t.Errorf("repeat: %+v %v", r, err)
	}
	u.Public = false
	if r, err := sy.ResyncComment(ctx, u); err != nil || r.Outcome != CommentUpdated || !r.Unpublished {
		t.Errorf("unpublish: %+v %v", r, err)
	}
	u.Public, u.Silent, u.CommentID = true, false, 6
	if r, err := sy.ResyncComment(ctx, u); err != nil || !r.Published || len(m.events) != 1 {
		t.Errorf("notify: %+v %v events %d", r, err, len(m.events))
	}
	if r, _ := sy.ResyncComment(ctx, CommentUpdate{FromBot: true, Repo: "demo/api", Number: 12, CommentID: 7}); r.Outcome != CommentSkipped {
		t.Errorf("bot: %+v", r)
	}
}

// 只允许项目范围内的 reviewer / admin；无关联 Issue 为冲突（REQ-011）。
func TestResyncTarget(t *testing.T) {
	m := newMem(
		Ticket{ID: 1, ProjectKey: "demo", Repo: "demo/api", IssueNumber: 12, Status: StatusOpen, UserID: "u1"},
		Ticket{ID: 2, ProjectKey: "demo", Repo: "demo/api", Status: StatusPendingTriage, UserID: "u1"},
		Ticket{ID: 3, ProjectKey: "other", Repo: "other/svc", IssueNumber: 3, Status: StatusOpen, UserID: "u3"},
	)
	s := newService(t, m, nil)
	ctx := context.Background()
	rev := auth.Principal{ID: "r1", Role: auth.RoleReviewer, Projects: []string{"demo"}}
	adm := auth.Principal{ID: "root", Role: auth.RoleAdmin, Projects: []string{"demo", "other"}}
	for _, c := range []struct {
		name string
		p    auth.Principal
		id   int64
		want error
	}{
		{"reviewer in project", rev, 1, nil},
		{"admin other project", adm, 3, nil},
		{"reviewer outside project", rev, 3, ErrNotFound},
		{"missing", rev, 99, ErrNotFound},
		{"no issue", rev, 2, ErrConflict},
		{"submitter", alice, 1, ErrForbidden},
		{"agent", auth.Principal{ID: "a1", Role: auth.RoleAgent}, 1, ErrForbidden},
		{"unknown role", auth.Principal{ID: "x", Role: "guest"}, 1, ErrForbidden},
		{"forbidden before lookup", alice, 99, ErrForbidden},
	} {
		got, err := s.ResyncTarget(ctx, c.p, c.id)
		if c.want == nil {
			if err != nil || got == nil || got.ID != c.id {
				t.Errorf("%s: %v %v", c.name, got, err)
			}
			continue
		}
		if !errors.Is(err, c.want) || got != nil {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
}
