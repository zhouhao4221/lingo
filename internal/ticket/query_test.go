package ticket

import (
	"context"
	"errors"
	"testing"

	"github.com/zhouhao4221/lingo/internal/auth"
)

// Search / Stats 按角色填写范围，调用方传入的范围被覆盖（与 Visible 一致）。
func TestSearchScopesByRole(t *testing.T) {
	m := newMem()
	s := newService(t, m, nil)
	ctx := context.Background()
	forged := SearchQuery{UserID: "someone", Scope: Scope{All: true}}
	for _, c := range []struct {
		name    string
		p       auth.Principal
		project string
		mine    bool
		userID  string
		fwd     bool
		all     bool
	}{
		{"external", alice, "", false, "u1", false, true},
		{"external mine", alice, "", true, "u1", false, true},
		{"agent", agent, "", false, "", true, true},
		{"reviewer", reviewer, "", false, "", false, false},
		{"reviewer mine", reviewer, "", true, reviewer.ID, false, false},
		{"reviewer project", reviewer, "demo", false, "", false, false},
	} {
		m.lastSearch = nil
		if _, err := s.Search(ctx, c.p, c.project, c.mine, forged); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := m.lastSearch
		if got == nil {
			t.Fatalf("%s: store not called", c.name)
		}
		if got.UserID != c.userID || got.ForwardedOnly != c.fwd || got.Scope.All != c.all || got.Limit != DefaultLimit {
			t.Errorf("%s: query = %+v", c.name, *got)
		}
	}
	// 项目不在范围内：不查询，结果为空
	m.lastSearch = nil
	if ts, err := s.Search(ctx, reviewer, "other", false, SearchQuery{}); err != nil || ts != nil || m.lastSearch != nil {
		t.Errorf("out of scope: %v %v %v", ts, err, m.lastSearch)
	}
	// agent 不提交工单：mine 为空
	m.lastSearch = nil
	if _, err := s.Stats(ctx, agent, "", true, SearchQuery{}); err != nil || m.lastSearch != nil {
		t.Errorf("agent mine: %v %v", err, m.lastSearch)
	}
	for _, q := range []SearchQuery{{Type: "x"}, {Statuses: []string{"nope"}}, {TimeField: "x"}} {
		if _, err := s.Search(ctx, reviewer, "", false, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", q, err)
		}
	}
	if _, err := s.Search(ctx, auth.Principal{ID: "x", Role: "guest"}, "", false, SearchQuery{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("guest: %v", err)
	}
	if _, err := s.Search(ctx, auth.Principal{Role: auth.RoleExternal}, "", false, SearchQuery{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("external without id: %v", err)
	}
}
