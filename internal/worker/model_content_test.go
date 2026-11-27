package worker

import (
	"testing"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/ticket"
	"github.com/zhouhao4221/lingo/internal/webhook"
)

// REQ-012：模型准入（Model）只按原始正文标记判定，与 ReplyMode 无关；Webhook、对账与历史回补共用 commentUpdate。
func TestCommentUpdateModelIndependentOfReplyMode(t *testing.T) {
	cases := []struct {
		body  string
		model bool
	}{
		{"普通评论", false},
		{"/reply 已修复", true},
		{"  \n/reply：已修复", true},
		{"说明 " + gitea.PublicMarker, true},
		{"/replyx 不是命令", false},
		{"引用 /reply 不在开头", false},
	}
	for _, mode := range []webhook.ReplyMode{webhook.ReplyMarked, webhook.ReplyAll, ""} {
		w := newResyncWorker(t, mode, "lingo-bot", &commentLister{}, &fakeTickets{}, true)
		for _, c := range cases {
			u := w.commentUpdate(ticket.CommentCreated, "demo/api", 1, &gitea.Comment{ID: 1, Body: c.body, User: &gitea.User{Login: "dev"}}, "")
			if u.Model != c.model {
				t.Errorf("mode %q body %q: model = %v", mode, c.body, u.Model)
			}
			if wantPublic := mode == webhook.ReplyAll || c.model; u.Public != wantPublic {
				t.Errorf("mode %q body %q: public = %v", mode, c.body, u.Public)
			}
		}
	}
}
