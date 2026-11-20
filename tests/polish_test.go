package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo"
)

func TestNeedInfoLabelRemovedAfterSupplement(t *testing.T) {
	g := &giteaServer{templates: "[]", labels: []map[string]any{{"id": 77, "name": "lingo/review/need-info"}}}
	l, db, _ := newTicketLingo(t, g, nil, map[lingo.TicketType]bool{lingo.TicketBug: true})
	ctx := context.Background()
	tk, err := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketBug, Content: "崩溃"})
	if err != nil {
		t.Fatal(err)
	}
	runLingo(t, l)
	waitTicket(t, l, alice, tk.ID, "open")
	if _, err := db.Exec("UPDATE tickets SET status = 'need_info' WHERE id = $1", tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AddTicketComment(ctx, alice, tk.ID, "复现步骤：点击导出", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		for _, e := range g.edits {
			if e == "DELETE /repos/demo/api/issues/40/labels/77" {
				return true
			}
		}
		return false
	})
	if got, _ := l.GetTicket(ctx, alice, tk.ID); got.Status != "in_progress" {
		t.Errorf("status = %s", got.Status)
	}
}

func TestMergedSubmitterFollowsTarget(t *testing.T) {
	g := &giteaServer{templates: "[]"}
	l, _, _ := newTicketLingo(t, g, nil, nil)
	ctx := context.Background()
	bob := lingo.User{ID: "u2", Name: "Bob", Role: lingo.RoleExternal}
	target, _ := l.CreateTicket(ctx, alice, lingo.TicketInput{Type: lingo.TicketFeature, Content: "支持导出 Excel"})
	dup, _ := l.CreateTicket(ctx, bob, lingo.TicketInput{Type: lingo.TicketFeature, Content: "能导出表格吗"})
	if _, err := l.MergeTicket(ctx, rita, dup.ID, target.ID, "重复"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AnswerTicket(ctx, rita, target.ID, "已支持，在设置 > 导出"); err != nil {
		t.Fatal(err)
	}
	evs, err := l.ListEvents(ctx, bob, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		got = append(got, string(e.Kind)+":"+e.Status)
		if e.UserID != "u2" {
			t.Errorf("bob sees another user's event: %+v", e)
		}
	}
	// 自己的：提交回执、合并；目标工单答复后的进展转给 bob
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "ticket.status:merged") || !strings.HasSuffix(joined, "ticket.status:answered") {
		t.Errorf("bob events = %s", joined)
	}
	last := evs[len(evs)-1]
	if last.TicketID != dup.ID || !strings.Contains(last.Body, "#"+jsonNum(target.ID)) || !strings.Contains(last.Body, "已支持") {
		t.Errorf("fan-out event = %+v", last)
	}
	// alice 看不到 bob 的转发
	aev, _ := l.ListEvents(ctx, alice, 0, 0)
	for _, e := range aev {
		if e.UserID != "u1" {
			t.Errorf("alice sees %+v", e)
		}
	}
}
