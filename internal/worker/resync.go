package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/ticket"
)

var (
	// ErrNotReady 表示 Gitea 尚未配置（或未提供 Issue 读接口），不能重新同步。
	ErrNotReady = errors.New("worker: gitea not configured")
	// ErrBotUnknown 表示机器人账号尚未获取：无法区分灵工自己发出的评论，稍后重试。
	ErrBotUnknown = errors.New("worker: gitea bot account not yet known")
)

// CommentResync 是一次历史评论重新同步的统计。
type CommentResync struct {
	Fetched     int // Gitea 上该 Issue 的评论数（含机器人评论）
	Bot         int // 机器人评论（灵工转发的补充等），跳过
	Added       int
	Updated     int
	Unchanged   int
	Skipped     int // Issue 已不关联该工单等
	Published   int // 由不可见变为对提交人可见
	Unpublished int // 由对外变为内部
}

// ResyncIssueComments 按当前 ReplyMode 与机器人账号重读 Issue 的全部评论，逐条走与 Webhook / 对账相同的
// commentUpdate → ApplyComment 逻辑（机器人过滤、/reply 前缀、去重、乱序与已删除判断），只写入工单 ticketID；
// notify 为假时新变为对外的评论不发事件。幂等：重复执行结果为 Unchanged。不根据"列表中缺少"推断删除。
func (w *Worker) ResyncIssueComments(ctx context.Context, ticketID int64, repo string, number int64, notify bool) (CommentResync, error) {
	var out CommentResync
	if !w.ready() || w.o.Issues == nil {
		return out, ErrNotReady
	}
	if w.o.BotLogin() == "" {
		return out, ErrBotUnknown
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || number <= 0 {
		return out, fmt.Errorf("worker: invalid issue %s#%d", repo, number)
	}
	cs, err := w.o.Issues.ListIssueComments(ctx, owner, name, number, time.Time{})
	if err != nil {
		return out, err
	}
	out.Fetched = len(cs)
	for _, c := range cs {
		if c == nil {
			continue
		}
		u := w.commentUpdate(ticket.CommentEdited, repo, number, c, "")
		if u.FromBot {
			out.Bot++
			continue
		}
		u.DeliveryID, u.TicketID, u.Silent = "resync", ticketID, !notify
		r, err := w.o.Tickets.ResyncComment(ctx, u)
		if err != nil {
			return out, err
		}
		switch r.Outcome {
		case ticket.CommentAdded:
			out.Added++
		case ticket.CommentUpdated:
			out.Updated++
		case ticket.CommentUnchanged:
			out.Unchanged++
		default:
			out.Skipped++
		}
		if r.Published {
			out.Published++
		}
		if r.Unpublished {
			out.Unpublished++
		}
	}
	return out, nil
}
