package ticket

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/zhouhao4221/lingo/internal/auth"
)

// REQ-004：external 被放宽可见的同事工单只读——可查看、检索，不能留言、补视角，看不到附件。
func TestPeersReadOnly(t *testing.T) {
	m := newMem()
	s := newService(t, m, nil)
	ctx := context.Background()
	tk, err := s.Create(ctx, bob, NewTicket{Project: "demo", Type: TypeFeature, Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	peer := alice
	peer.Peers = []string{bob.ID}
	outside := peer
	outside.Projects = []string{"other"}

	if !Readable(peer, tk) || Visible(peer, tk) {
		t.Errorf("readable=%v visible=%v", Readable(peer, tk), Visible(peer, tk))
	}
	if Readable(alice, tk) || Readable(outside, tk) {
		t.Error("readable without peers or outside projects")
	}
	ag := agent
	ag.Peers = []string{bob.ID}
	if Readable(ag, tk) { // 只对 external 生效
		t.Error("agent with peers")
	}
	if _, err := s.View(ctx, peer, tk.ID); err != nil {
		t.Errorf("view: %v", err)
	}
	if _, err := s.View(ctx, outside, tk.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("view outside projects: %v", err)
	}
	if _, err := s.Get(ctx, peer, tk.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get (write path): %v", err)
	}
	if _, err := s.Comments(ctx, peer, tk.ID); err != nil {
		t.Errorf("comments: %v", err)
	}
	m.attachStub = []Attachment{{ID: "a1"}}
	if _, _, as, err := s.Detail(ctx, peer, tk.ID); err != nil || as != nil {
		t.Errorf("detail attachments for peer: %v %v", as, err)
	}
	if _, _, as, err := s.Detail(ctx, bob, tk.ID); err != nil || len(as) != 1 {
		t.Errorf("detail attachments for submitter: %v %v", as, err)
	}
	if _, err := s.AddComment(ctx, peer, tk.ID, "x", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("peer comment: %v", err)
	}
	if _, err := s.ListPerspectives(ctx, peer, tk.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("peer perspectives: %v", err)
	}
}

// 检索与可见列表：external 含本人与 Peers；mine 只含本人；其他角色不受 Peers 影响。
func TestPeersSearchScope(t *testing.T) {
	m := newMem()
	s := newService(t, m, nil)
	ctx := context.Background()
	peer := alice
	peer.Peers = []string{"u2", "u3"}
	if _, err := s.Search(ctx, peer, "", false, SearchQuery{}); err != nil {
		t.Fatal(err)
	}
	if q := m.lastSearch; q.UserID != "" || !slices.Equal(q.UserIDs, []string{"u1", "u2", "u3"}) {
		t.Errorf("search = %+v", *q)
	}
	if _, err := s.Stats(ctx, peer, "", true, SearchQuery{UserIDs: []string{"forged"}}); err != nil {
		t.Fatal(err)
	}
	if q := m.lastSearch; q.UserID != "u1" || q.UserIDs != nil {
		t.Errorf("mine = %+v", *q)
	}
	if _, err := s.ListVisible(ctx, peer, "", VisibleQuery{UserIDs: []string{"forged"}}); err != nil {
		t.Fatal(err)
	}
	if q := m.lastVisible; !slices.Equal(q.UserIDs, []string{"u1", "u2", "u3"}) || q.Limit != DefaultLimit {
		t.Errorf("visible = %+v", *q)
	}
	rv := reviewer
	rv.Peers = []string{"u2"}
	if _, err := s.ListVisible(ctx, rv, "", VisibleQuery{}); err != nil {
		t.Fatal(err)
	}
	if q := m.lastVisible; !slices.Equal(q.UserIDs, []string{rv.ID}) {
		t.Errorf("reviewer visible = %+v", *q)
	}
	m.lastVisible = nil
	if ts, err := s.ListVisible(ctx, rv, "other", VisibleQuery{}); err != nil || ts != nil || m.lastVisible != nil {
		t.Errorf("out of scope: %v %v", ts, err)
	}
	if _, err := s.ListVisible(ctx, auth.Principal{Role: auth.RoleExternal}, "", VisibleQuery{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("no id: %v", err)
	}
	if _, err := s.ListVisible(ctx, peer, "", VisibleQuery{Status: "nope"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad status: %v", err)
	}
}
