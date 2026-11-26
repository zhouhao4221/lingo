package ticket

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
)

// 合成数据（待办 #789）：待确认工单的 triage_since 与 ID 顺序相反、含并列时间；提交人姓名有大小写与子串差异。
func triageFixture() *memStore {
	base := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	at := func(min int) *time.Time { t := base.Add(time.Duration(min) * time.Minute); return &t }
	return newMem(
		Ticket{ID: 1, ProjectKey: "demo", Type: TypeBug, Title: "a", Status: StatusPendingTriage, UserID: "u1", UserName: "张三", TriageSince: at(30)},
		Ticket{ID: 2, ProjectKey: "demo", Type: TypeBug, Title: "b", Status: StatusPendingTriage, UserID: "u2", UserName: "Alice Wang", TriageSince: at(10)},
		Ticket{ID: 3, ProjectKey: "demo", Type: TypeFeature, Title: "c", Status: StatusPendingTriage, UserID: "u1", UserName: "张三丰", TriageSince: at(10)},
		Ticket{ID: 4, ProjectKey: "demo", Type: TypeBug, Title: "d", Status: StatusOpen, UserID: "u2", UserName: "alice wang"},
		Ticket{ID: 5, ProjectKey: "demo", Type: TypeBug, Title: "e", Status: StatusPendingTriage, UserID: "u3", UserName: "李四", TriageSince: at(0)},
		Ticket{ID: 6, ProjectKey: "other", Type: TypeBug, Title: "f", Status: StatusPendingTriage, UserID: "u1", UserName: "张三", TriageSince: at(-60)},
		Ticket{ID: 7, ProjectKey: "demo", Type: TypeBug, Title: "g", Status: StatusPendingTriage, UserID: "u2", UserName: "Alice Wang", TriageSince: at(10)},
		Ticket{ID: 8, ProjectKey: "demo", Type: TypeBug, Title: "h", Status: StatusAnswered, UserID: "u3", UserName: "李四"},
	)
}

// 待确认时间正序：只含待确认，并列时间按 ID 正序；游标逐页拼回全集、无重无漏；Total 固定。
func TestListTriageOrderAndCursor(t *testing.T) {
	s := newService(t, triageFixture(), nil)
	ctx := context.Background()
	rev := auth.Principal{ID: "r1", Role: auth.RoleReviewer, Projects: []string{"demo"}}
	want := []int64{5, 2, 3, 7, 1}
	ts, total, more, err := s.List(ctx, rev, "", false, SearchQuery{Order: OrderTriageAsc})
	if err != nil || !slices.Equal(listIDs(ts), want) || total != 5 || more {
		t.Fatalf("all: %v total %d more %v err %v", listIDs(ts), total, more, err)
	}
	for _, size := range []int{1, 2, 3, 5} {
		var got []int64
		q, pages := SearchQuery{Order: OrderTriageAsc, Limit: size}, 0
		for {
			ts, total, more, err := s.List(ctx, rev, "", false, q)
			if err != nil || total != 5 {
				t.Fatalf("size %d page %d: total %d err %v", size, pages, total, err)
			}
			got = append(got, listIDs(ts)...)
			pages++
			if !more {
				break
			}
			if q.AfterTriage, q.AfterID, err = ParseTriageCursor(TriageCursor(ts[len(ts)-1])); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(got, want) || pages != (5+size-1)/size {
			t.Errorf("size %d: %v in %d pages", size, got, pages)
		}
	}
	// 组合：类型 + 提交人姓名
	if ts, total, _, err := s.List(ctx, rev, "", false, SearchQuery{Order: OrderTriageAsc, Type: TypeBug, SubmitterName: "ALICE"}); err != nil ||
		!slices.Equal(listIDs(ts), []int64{2, 7}) || total != 2 {
		t.Errorf("combined: %v %d %v", listIDs(ts), total, err)
	}
	// 与状态筛选取交集：非待确认状态为空
	if ts, total, _, err := s.List(ctx, rev, "", false, SearchQuery{Order: OrderTriageAsc, Statuses: []string{"open"}}); err != nil || len(ts) != 0 || total != 0 {
		t.Errorf("open + triage order: %v %d %v", listIDs(ts), total, err)
	}
	// external 仍只含本人；agent 只看已转出，故为空
	ext := auth.Principal{ID: "u1", Role: auth.RoleExternal, Projects: []string{"demo", "other"}}
	if ts, _, _, err := s.List(ctx, ext, "", true, SearchQuery{Order: OrderTriageAsc}); err != nil || !slices.Equal(listIDs(ts), []int64{6, 3, 1}) {
		t.Errorf("external: %v %v", listIDs(ts), err)
	}
	ag := auth.Principal{ID: "a1", Role: auth.RoleAgent, Projects: []string{"demo"}}
	if ts, total, _, err := s.List(ctx, ag, "", false, SearchQuery{Order: OrderTriageAsc}); err != nil || len(ts) != 0 || total != 0 {
		t.Errorf("agent: %v %d %v", listIDs(ts), total, err)
	}
}

// 提交人姓名：子串、不区分大小写，只在可见范围内收窄；默认排序不变。
func TestListSubmitterName(t *testing.T) {
	s := newService(t, triageFixture(), nil)
	ctx := context.Background()
	rev := auth.Principal{ID: "r1", Role: auth.RoleReviewer, Projects: []string{"demo"}}
	for _, c := range []struct {
		name string
		want []int64
	}{
		{"张三", []int64{3, 1}}, // 不含 other 项目的 6
		{"三丰", []int64{3}},
		{"alice wang", []int64{7, 4, 2}},
		{"无此人", []int64{}},
	} {
		ts, total, _, err := s.List(ctx, rev, "", false, SearchQuery{SubmitterName: c.name})
		if err != nil || !slices.Equal(listIDs(ts), c.want) || total != len(c.want) {
			t.Errorf("%s: %v %d %v, want %v", c.name, listIDs(ts), total, err, c.want)
		}
	}
	// external 按他人姓名筛选也看不到他人的
	ext := auth.Principal{ID: "u1", Role: auth.RoleExternal, Projects: []string{"demo"}}
	if ts, _, _, err := s.List(ctx, ext, "", true, SearchQuery{SubmitterName: "Alice"}); err != nil || len(ts) != 0 {
		t.Errorf("external other name: %v %v", listIDs(ts), err)
	}
}

func TestListTriageRejects(t *testing.T) {
	s := newService(t, triageFixture(), nil)
	ctx := context.Background()
	rev := auth.Principal{ID: "r1", Role: auth.RoleReviewer, Projects: []string{"demo"}}
	for _, c := range []struct {
		name string
		q    SearchQuery
	}{
		{"bad sort", SearchQuery{Order: "created_asc"}},
		{"before with triage", SearchQuery{Order: OrderTriageAsc, BeforeID: 3}},
		{"cursor without sort", SearchQuery{AfterID: 3, AfterTriage: time.Now()}},
		{"name too long", SearchQuery{SubmitterName: strings.Repeat("名", MaxListNameRunes+1)}},
	} {
		if _, _, _, err := s.List(ctx, rev, "", false, c.q); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// 非 base64、空、"abc:1"、"1:0"（ID 须为正）
	for _, bad := range []string{"x", "!!", TriageCursor(nil), "YWJjOjE", "MTow"} {
		if _, _, err := ParseTriageCursor(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("cursor %q: %v", bad, err)
		}
	}
	if _, id, err := ParseTriageCursor("MTox"); err != nil || id != 1 { // "1:1"
		t.Errorf("valid cursor: %v %v", id, err)
	}
	tm := time.Date(2026, 11, 1, 8, 0, 0, 123456000, time.UTC)
	if at, id, err := ParseTriageCursor(TriageCursor(&Ticket{ID: 42, TriageSince: &tm})); err != nil || id != 42 || !at.Equal(tm) {
		t.Errorf("roundtrip: %v %d %v", at, id, err)
	}
}
