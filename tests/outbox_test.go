package tests

import (
	"context"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

func emit(t *testing.T, s *store.Store, es ...notify.Event) {
	t.Helper()
	if err := s.InTx(context.Background(), func(tx ticket.Tx) error {
		for _, e := range es {
			if err := tx.Emit(context.Background(), e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEmitWithoutNotifyOnlyWritesEvents(t *testing.T) {
	db := testDB(t, true)
	id := seedTicket(t, db, "open")
	s, _ := store.New(db)
	emit(t, s, notify.Event{Kind: notify.TicketStatus, TicketID: id, Status: "in_progress"})
	var events, outbox int
	if err := db.QueryRow("SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM outbox)").Scan(&events, &outbox); err != nil {
		t.Fatal(err)
	}
	if events != 1 || outbox != 0 {
		t.Errorf("events %d outbox %d", events, outbox)
	}
}

func TestClaimNotificationsPerTicketOrder(t *testing.T) {
	db := testDB(t, true)
	a := seedTicket(t, db, "open")
	var b int64
	if err := db.QueryRow(`INSERT INTO tickets (user_id, project_id, type, title, status)
		SELECT 'u2', project_id, 'bug', 'B', 'pending_triage' FROM tickets WHERE id = $1 RETURNING id`, a).Scan(&b); err != nil {
		t.Fatal(err)
	}
	s, _ := store.New(db, store.WithNotify())
	ctx := context.Background()
	emit(t, s,
		notify.Event{Kind: notify.TicketStatus, ProjectID: 1, TicketID: a, TicketType: "bug", Title: "登录失败", Status: "in_progress", PrevStatus: "open", UserID: "u1", URL: "http://g/demo/api/issues/12"},
		notify.Event{Kind: notify.TicketComment, TicketID: a, Body: "已修复"},
		notify.Event{Kind: notify.TriageRequired, TicketID: b},
	)
	first, err := s.ClaimNotifications(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// a 的第二条须等第一条完成
	if len(first) != 2 || first[0].Event.TicketID != a || first[1].Event.TicketID != b {
		t.Fatalf("first claim = %+v", first)
	}
	e := first[0].Event
	if e.ID == 0 || e.At.IsZero() || e.ProjectKey != "demo" || e.Title != "登录失败" || e.TicketType != "bug" || e.URL == "" || e.PrevStatus != "open" {
		t.Errorf("event = %+v", e)
	}
	if err := s.RetryNotification(ctx, first[0], "notifier down", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteNotification(ctx, first[1]); err != nil {
		t.Fatal(err)
	}
	second, _ := s.ClaimNotifications(ctx, 10, time.Minute)
	if len(second) != 1 || second[0].Event.Kind != notify.TicketStatus || second[0].Attempts != 2 {
		t.Fatalf("second claim = %+v", second)
	}
	if err := s.FailNotification(ctx, second[0], "gave up"); err != nil {
		t.Fatal(err)
	}
	// 放弃的通知不再阻塞同一工单的后续通知
	third, _ := s.ClaimNotifications(ctx, 10, time.Minute)
	if len(third) != 1 || third[0].Event.Kind != notify.TicketComment || third[0].Event.Body != "已修复" {
		t.Fatalf("third claim = %+v", third)
	}
	if err := s.CompleteNotification(ctx, second[0]); err == nil {
		t.Error("completing a failed notification succeeded")
	}
}

func TestGiteaCommentStore(t *testing.T) {
	db := testDB(t, true)
	id := seedTicket(t, db, "open")
	s, _ := store.New(db)
	ctx := context.Background()
	t1 := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	save := func(c ticket.Comment) (prev *ticket.Comment) {
		t.Helper()
		if err := s.InTx(ctx, func(tx ticket.Tx) error {
			var err error
			prev, err = tx.SaveGiteaComment(ctx, c)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return prev
	}
	if p := save(ticket.Comment{TicketID: id, GiteaID: 9, Body: "v1", At: t1}); p != nil {
		t.Errorf("new comment prev = %+v", p)
	}
	if p := save(ticket.Comment{TicketID: id, GiteaID: 9, Body: "v2", Public: true, At: t1.Add(time.Minute)}); p == nil || p.Body != "v1" || p.Public {
		t.Errorf("edit prev = %+v", p)
	}
	// 乱序到达的旧编辑不覆盖
	save(ticket.Comment{TicketID: id, GiteaID: 9, Body: "old", At: t1})
	var body string
	if err := db.QueryRow("SELECT body FROM ticket_comments WHERE gitea_comment_id = 9").Scan(&body); err != nil || body != "v2" {
		t.Errorf("body = %q %v", body, err)
	}
	if err := s.InTx(ctx, func(tx ticket.Tx) error { return tx.DeleteGiteaComment(ctx, 9, t1.Add(2*time.Minute)) }); err != nil {
		t.Fatal(err)
	}
	if p := save(ticket.Comment{TicketID: id, GiteaID: 9, Body: "late", Public: true, At: t1.Add(3 * time.Minute)}); p == nil || !p.Public {
		t.Errorf("edit after delete prev = %+v", p)
	}
	if err := db.QueryRow("SELECT body FROM ticket_comments WHERE gitea_comment_id = 9").Scan(&body); err != nil || body != "v2" {
		t.Errorf("deleted comment rewritten: %q %v", body, err)
	}
}
