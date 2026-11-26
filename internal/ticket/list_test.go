package ticket

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
)

func listIDs(ts []*Ticket) []int64 {
	out := []int64{}
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

// 合成数据：两个项目、多个提交人与状态（REQ-010）。
func listFixture() *memStore {
	return newMem(
		Ticket{ID: 1, ProjectKey: "demo", Repo: "demo/api", Type: TypeBug, Title: "登录白屏", Body: "点击登录后空白", Status: StatusPendingTriage, UserID: "u1"},
		Ticket{ID: 2, ProjectKey: "demo", Repo: "demo/web", Type: TypeBug, Title: "列表报错", Body: "登录后列表 500", Status: StatusOpen, UserID: "u2"},
		Ticket{ID: 3, ProjectKey: "demo", Repo: "demo/api", Type: TypeFeature, Title: "导出 Excel", Body: "希望导出", Status: StatusOpen, UserID: "u1"},
		Ticket{ID: 4, ProjectKey: "other", Repo: "other/svc", Type: TypeBug, Title: "同步失败", Body: "x", Status: StatusOpen, UserID: "u3"},
		Ticket{ID: 5, ProjectKey: "other", Repo: "other/svc", Type: TypeConsult, Title: "怎么开票", Body: "y", Status: StatusSubmitted, UserID: "u1"},
		Ticket{ID: 6, ProjectKey: "demo", Repo: "demo/web", Type: TypeFeature, Title: "暗色模式", Body: "z", Status: StatusPendingTriage, UserID: "r1"},
		Ticket{ID: 7, ProjectKey: "demo", Repo: "demo/api", Type: TypeBug, Title: "已驳回的", Body: "w", Status: StatusRejected, UserID: "u2"},
	)
}

// 角色范围与 Search / Readable 一致；跨项目隔离；越权项目为空而非报错。
func TestListRoleScopes(t *testing.T) {
	s := newService(t, listFixture(), nil)
	ctx := context.Background()
	both := []string{"demo", "other"}
	ext := auth.Principal{ID: "u1", Role: auth.RoleExternal, Projects: both}
	extPeers := ext
	extPeers.Peers = []string{"u2"}
	ag := auth.Principal{ID: "a1", Role: auth.RoleAgent, Projects: []string{"demo"}}
	rev := auth.Principal{ID: "r1", Role: auth.RoleReviewer, Projects: []string{"demo"}}
	adm := auth.Principal{ID: "root", Role: auth.RoleAdmin, Projects: []string{"other"}}
	for _, c := range []struct {
		name    string
		p       auth.Principal
		project string
		mine    bool
		want    []int64
	}{
		{"external mine", ext, "", true, []int64{5, 3, 1}},
		{"external mine project", ext, "demo", true, []int64{3, 1}},
		{"external without peers", ext, "", false, []int64{5, 3, 1}},
		{"external with peers", extPeers, "demo", false, []int64{7, 3, 2, 1}},
		{"external peers mine", extPeers, "demo", true, []int64{3, 1}},
		{"external peers outside projects", auth.Principal{ID: "u1", Role: auth.RoleExternal, Projects: []string{"other"}, Peers: []string{"u2"}}, "", false, []int64{5}},
		{"external unknown project", ext, "nope", true, []int64{}},
		{"agent forwarded only", ag, "", false, []int64{3, 2}},
		{"agent other project", ag, "other", false, []int64{}},
		{"agent mine", ag, "", true, []int64{}},
		{"reviewer project scope", rev, "", false, []int64{7, 6, 3, 2, 1}},
		{"reviewer outside project", rev, "other", false, []int64{}},
		{"reviewer mine", rev, "", true, []int64{6}},
		{"admin project scope", adm, "", false, []int64{5, 4}},
		{"admin outside project", adm, "demo", false, []int64{}},
	} {
		ts, total, more, err := s.List(ctx, c.p, c.project, c.mine, SearchQuery{})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := listIDs(ts); !slices.Equal(got, c.want) || total != len(c.want) || more {
			t.Errorf("%s: ids %v total %d more %v, want %v", c.name, got, total, more, c.want)
		}
	}
}

// 调用方伪造的范围字段被覆盖：不能借 SearchQuery 扩大可见范围。
func TestListIgnoresForgedScope(t *testing.T) {
	s := newService(t, listFixture(), nil)
	ctx := context.Background()
	forged := SearchQuery{Scope: Scope{All: true}, UserIDs: []string{"u2", "u3"}, ForwardedOnly: false}
	ext := auth.Principal{ID: "u1", Role: auth.RoleExternal, Projects: []string{"demo"}}
	if ts, _, _, err := s.List(ctx, ext, "", true, forged); err != nil || !slices.Equal(listIDs(ts), []int64{3, 1}) {
		t.Errorf("external forged = %v %v", listIDs(ts), err)
	}
	ag := auth.Principal{ID: "a1", Role: auth.RoleAgent, Projects: []string{"demo"}}
	if ts, _, _, err := s.List(ctx, ag, "", false, forged); err != nil || !slices.Equal(listIDs(ts), []int64{3, 2}) {
		t.Errorf("agent forged = %v %v", listIDs(ts), err)
	}
}

// 筛选与分页：Total 不受游标与条数影响；hasMore 精确（多取一条判断）。
func TestListFiltersAndPaging(t *testing.T) {
	m := listFixture()
	s := newService(t, m, nil)
	ctx := context.Background()
	rev := auth.Principal{ID: "r1", Role: auth.RoleReviewer, Projects: []string{"demo"}}
	for _, c := range []struct {
		name string
		q    SearchQuery
		want []int64
	}{
		{"status", SearchQuery{Statuses: []string{"open"}}, []int64{3, 2}},
		{"statuses", SearchQuery{Statuses: []string{"open", "pending_triage"}}, []int64{6, 3, 2, 1}},
		{"type", SearchQuery{Type: TypeFeature}, []int64{6, 3}},
		{"repo", SearchQuery{Repo: "Demo/Web"}, []int64{6, 2}},
		{"keyword title or body", SearchQuery{Keywords: []string{"登录"}}, []int64{2, 1}},
		{"keywords all match", SearchQuery{Keywords: []string{"登录", "500"}}, []int64{2}},
		{"combined", SearchQuery{Type: TypeBug, Statuses: []string{"open"}, Keywords: []string{"列表"}}, []int64{2}},
		{"no match", SearchQuery{Keywords: []string{"不存在"}}, []int64{}},
	} {
		ts, total, more, err := s.List(ctx, rev, "", false, c.q)
		if err != nil || !slices.Equal(listIDs(ts), c.want) || total != len(c.want) || more {
			t.Errorf("%s: %v total %d more %v err %v, want %v", c.name, listIDs(ts), total, more, err, c.want)
		}
	}
	// 状态去重后传给 store
	if _, _, _, err := s.List(ctx, rev, "", false, SearchQuery{Statuses: []string{"open", "open", "rejected", "open"}}); err != nil ||
		!slices.Equal(m.lastSearch.Statuses, []string{"open", "rejected"}) {
		t.Errorf("dedupe statuses = %v %v", m.lastSearch.Statuses, err)
	}
	// 分页：5 张，每页 2
	var got []int64
	before, pages := int64(0), 0
	for {
		ts, total, more, err := s.List(ctx, rev, "", false, SearchQuery{BeforeID: before, Limit: 2})
		if err != nil || total != 5 {
			t.Fatalf("page %d: total %d err %v", pages, total, err)
		}
		got = append(got, listIDs(ts)...)
		pages++
		if !more {
			break
		}
		before = ts[len(ts)-1].ID
	}
	if !slices.Equal(got, []int64{7, 6, 3, 2, 1}) || pages != 3 {
		t.Errorf("paged = %v in %d pages", got, pages)
	}
	// 恰好整页：没有下一页
	if ts, _, more, _ := s.List(ctx, rev, "", false, SearchQuery{Limit: 5}); len(ts) != 5 || more {
		t.Errorf("exact page: %d more %v", len(ts), more)
	}
	// 条数默认 50、上限 200（多取一条）
	if _, _, _, _ = s.List(ctx, rev, "", false, SearchQuery{}); m.lastSearch.Limit != DefaultLimit+1 {
		t.Errorf("default limit = %d", m.lastSearch.Limit)
	}
	if _, _, _, _ = s.List(ctx, rev, "", false, SearchQuery{Limit: 1000}); m.lastSearch.Limit != MaxLimit+1 {
		t.Errorf("max limit = %d", m.lastSearch.Limit)
	}
}

// 无权限与非法条件。
func TestListRejects(t *testing.T) {
	s := newService(t, listFixture(), nil)
	ctx := context.Background()
	rev := auth.Principal{ID: "r1", Role: auth.RoleReviewer, Projects: []string{"demo"}}
	for _, c := range []struct {
		name string
		p    auth.Principal
		q    SearchQuery
		want error
	}{
		{"unknown role", auth.Principal{ID: "x", Role: "guest"}, SearchQuery{}, ErrForbidden},
		{"empty role", auth.Principal{ID: "x"}, SearchQuery{}, ErrForbidden},
		{"external without id", auth.Principal{Role: auth.RoleExternal}, SearchQuery{}, ErrForbidden},
		{"bad type", rev, SearchQuery{Type: "x"}, ErrInvalid},
		{"bad status", rev, SearchQuery{Statuses: []string{"done"}}, ErrInvalid},
		{"bad time field", rev, SearchQuery{TimeField: "x"}, ErrInvalid},
		{"too many keywords", rev, SearchQuery{Keywords: strings.Fields(strings.Repeat("k ", MaxListKeywords+1))}, ErrInvalid},
		{"keyword too long", rev, SearchQuery{Keywords: []string{strings.Repeat("长", MaxListKeywordRunes+1)}}, ErrInvalid},
	} {
		ts, total, _, err := s.List(ctx, c.p, "", false, c.q)
		if !errors.Is(err, c.want) || ts != nil || total != 0 {
			t.Errorf("%s: %v %v %d", c.name, err, ts, total)
		}
	}
	// 大量互不相同的非法状态：第一个非法值即返回，不做 O(n²) 去重
	many := make([]string, 100000)
	for i := range many {
		many[i] = "bad" + strconv.Itoa(i)
	}
	start := time.Now()
	if _, _, _, err := s.List(ctx, rev, "", false, SearchQuery{Statuses: many}); !errors.Is(err, ErrInvalid) {
		t.Errorf("many invalid statuses: %v", err)
	}
	// 合法值大量重复：线性完成
	dup := make([]string, 100000)
	for i := range dup {
		dup[i] = []string{"open", "rejected", "fixed"}[i%3]
	}
	if _, _, _, err := s.List(ctx, rev, "", false, SearchQuery{Statuses: dup}); err != nil {
		t.Errorf("many duplicate statuses: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("status dedupe took %v", d)
	}
	// 合法值在前、非法值在后：同样返回 ErrInvalid
	if _, _, _, err := s.List(ctx, rev, "", false, SearchQuery{Statuses: []string{"open", "open", "nope"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("trailing invalid: %v", err)
	}
	// 恰好上限的关键词数与长度可以
	ok := SearchQuery{Keywords: append(strings.Fields(strings.Repeat("k ", MaxListKeywords-1)), strings.Repeat("长", MaxListKeywordRunes))}
	if _, _, _, err := s.List(ctx, rev, "", false, ok); err != nil {
		t.Errorf("at limits: %v", err)
	}
}
