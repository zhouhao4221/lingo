package ticket

import (
	"context"
	"testing"
	"time"
)

// REQ-012：模型准入随评论保存；只有准入判定变化（如升级前同步的评论首次判定）时同样写入，但不留痕、不改可见性计数、
// 不通知；乱序旧版本不覆盖。
func TestApplyCommentStoresModelSilently(t *testing.T) {
	m := newMem(Ticket{ID: 1, ProjectKey: "demo", Repo: "demo/api", IssueNumber: 12, Status: StatusOpen, UserID: "u1"})
	sy, err := NewSync(SyncOptions{Store: m})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.comments[5] = Comment{TicketID: 1, GiteaID: 5, AuthorName: "dev", Body: "内部讨论", Public: true, At: t0} // 升级前同步：未判定
	hist, events := len(m.history), len(m.events)
	u := CommentUpdate{Change: CommentEdited, Repo: "demo/api", Number: 12, CommentID: 5, Author: "dev", Body: "内部讨论", Public: true,
		Model: false, At: t0}
	if r, err := sy.ResyncComment(ctx, u); err != nil || r.Outcome != CommentUnchanged || r.Published || r.Unpublished {
		t.Fatalf("classify: %+v %v", r, err)
	}
	if len(m.history) != hist || len(m.events) != events {
		t.Errorf("classification left traces: history %d→%d events %d→%d", hist, len(m.history), events, len(m.events))
	}
	u.Body, u.Model, u.At = "已修复", true, t0.Add(time.Minute)
	if r, err := sy.ResyncComment(ctx, u); err != nil || r.Outcome != CommentUpdated || !m.comments[5].Model {
		t.Fatalf("edit to marked: %+v %v %+v", r, err, m.comments[5])
	}
	old := u
	old.Body, old.Model, old.At = "旧版本", false, t0
	if r, err := sy.ResyncComment(ctx, old); err != nil || r.Outcome != CommentUnchanged || !m.comments[5].Model || m.comments[5].Body != "已修复" {
		t.Errorf("stale overwrote: %+v %v %+v", r, err, m.comments[5])
	}
	if got := ModelComments([]Comment{{ID: 1, Model: true}, {ID: 2}, {ID: 3, Model: true}}); len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Errorf("ModelComments = %+v", got)
	}
}
