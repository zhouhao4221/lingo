package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/ticket"
	"github.com/zhouhao4221/lingo/internal/webhook"
)

// commentLister 只实现 ListIssueComments，其余 IssueWriter 方法不应被调用。
type commentLister struct {
	IssueWriter
	cs  []*gitea.Comment
	err error
	got []string
}

func (c *commentLister) ListIssueComments(_ context.Context, owner, repo string, index int64, since time.Time) ([]*gitea.Comment, error) {
	c.got = append(c.got, owner+"/"+repo)
	if !since.IsZero() {
		return nil, errors.New("resync must read all comments")
	}
	return c.cs, c.err
}

func newResyncWorker(t *testing.T, mode webhook.ReplyMode, bot string, g *commentLister, tk *fakeTickets, ready bool) *Worker {
	t.Helper()
	w, err := New(Options{Deliveries: &fakeQueue{}, Gitea: g, Tickets: tk, Issues: g, ReplyMode: mode,
		BotLogin: func() string { return bot }, Ready: func() bool { return ready }, Now: func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// 走与对账相同的 commentUpdate：机器人跳过、ReplyMode 判定、/reply 前缀；限定工单、默认不通知。
func TestResyncIssueComments(t *testing.T) {
	at := now.Add(-time.Hour)
	cs := []*gitea.Comment{
		{ID: 1, Body: "内部讨论", User: &gitea.User{Login: "dev"}, UpdatedAt: at},
		{ID: 2, Body: "/reply 已修复", User: &gitea.User{Login: "dev"}, UpdatedAt: at},
		{ID: 3, Body: "补充（灵工转发）", User: &gitea.User{Login: "Lingo-Bot"}, UpdatedAt: at},
	}
	for _, c := range []struct {
		mode   webhook.ReplyMode
		notify bool
		public []bool
		bodies []string
	}{
		{webhook.ReplyAll, false, []bool{true, true}, []string{"内部讨论", "已修复"}},
		{webhook.ReplyMarked, true, []bool{false, true}, []string{"内部讨论", "已修复"}},
	} {
		tk := &fakeTickets{}
		g := &commentLister{cs: cs}
		w := newResyncWorker(t, c.mode, "lingo-bot", g, tk, true)
		got, err := w.ResyncIssueComments(context.Background(), 7, "demo/api", 12, c.notify)
		if err != nil {
			t.Fatal(err)
		}
		if got.Fetched != 3 || got.Bot != 1 || got.Added != 2 || len(tk.comments) != 2 || g.got[0] != "demo/api" {
			t.Fatalf("%s: %+v %d", c.mode, got, len(tk.comments))
		}
		for i, u := range tk.comments {
			if u.TicketID != 7 || u.Silent == c.notify || u.DeliveryID != "resync" || u.Repo != "demo/api" || u.Number != 12 ||
				u.Public != c.public[i] || u.Body != c.bodies[i] || !u.At.Equal(at) || u.Change != ticket.CommentEdited {
				t.Errorf("%s: update %d = %+v", c.mode, i, u)
			}
		}
	}
	// 未配置、机器人账号未知、Gitea 读取失败
	if _, err := newResyncWorker(t, webhook.ReplyAll, "bot", &commentLister{}, &fakeTickets{}, false).
		ResyncIssueComments(context.Background(), 1, "demo/api", 1, false); !errors.Is(err, ErrNotReady) {
		t.Errorf("not ready: %v", err)
	}
	if _, err := newResyncWorker(t, webhook.ReplyAll, "", &commentLister{}, &fakeTickets{}, true).
		ResyncIssueComments(context.Background(), 1, "demo/api", 1, false); !errors.Is(err, ErrBotUnknown) {
		t.Errorf("bot unknown: %v", err)
	}
	boom := errors.New("gitea down")
	tk := &fakeTickets{}
	if _, err := newResyncWorker(t, webhook.ReplyAll, "bot", &commentLister{err: boom}, tk, true).
		ResyncIssueComments(context.Background(), 1, "demo/api", 1, false); !errors.Is(err, boom) || len(tk.comments) != 0 {
		t.Errorf("list error: %v", err)
	}
}
