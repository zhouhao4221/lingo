package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
	"github.com/zhouhao4221/lingo/internal/webhook"
)

// botRetry 是机器人账号未就绪时推迟处理的间隔。
const botRetry = 30 * time.Second

// ProcessWebhooks 领取并处理一批 Webhook 投递，返回领取到的条数。
func (w *Worker) ProcessWebhooks(ctx context.Context) (int, error) {
	claimed := time.Now()
	ds, err := w.o.Deliveries.ClaimDeliveries(ctx, w.o.BatchSize, w.o.Lease)
	if err != nil {
		return 0, err
	}
	budget := w.batchBudget(claimed)
	for i, d := range ds {
		switch {
		case ctx.Err() != nil:
			// 退出中：释放尚未开始的条目，交给其他实例或下次启动
			w.releaseDelivery(ctx, d, "worker shutting down")
		case i > 0 && time.Now().After(budget):
			w.releaseDelivery(ctx, d, "lease budget exhausted")
		default:
			w.handleDelivery(ctx, d)
		}
	}
	return len(ds), nil
}

func (w *Worker) releaseDelivery(ctx context.Context, d store.Delivery, reason string) {
	fctx, cancel := finishContext(ctx)
	defer cancel()
	w.finishDelivery(d, w.o.Deliveries.ReleaseDelivery(fctx, d, reason, w.o.Now()))
}

func (w *Worker) handleDelivery(ctx context.Context, d store.Delivery) {
	tctx, cancel := w.taskContext(ctx)
	err := w.processDelivery(tctx, d)
	cancel()
	fctx, fcancel := finishContext(ctx)
	defer fcancel()
	log := w.log.With("delivery", d.DeliveryID, "kind", d.Kind, "attempt", d.Attempts, "failures", d.Failures)
	var de *ticket.DeferError
	var pe *permanentError
	switch {
	case err == nil:
		w.finishDelivery(d, w.o.Deliveries.CompleteDelivery(fctx, d))
	case errors.As(err, &de):
		// 推迟不是失败，不消耗重试次数
		log.Debug("webhook delivery deferred", "until", de.Until, "reason", de.Reason)
		w.finishDelivery(d, w.o.Deliveries.ReleaseDelivery(fctx, d, de.Reason, de.Until))
	case errors.As(err, &pe) || d.Failures+1 >= w.o.MaxAttempts:
		// 放弃后由定时对账补齐其影响
		log.Error("webhook delivery abandoned", "err", err)
		w.finishDelivery(d, w.o.Deliveries.AbandonDelivery(fctx, d, err.Error()))
	default:
		retryAt := w.o.Now().Add(backoff(d.Failures + 1))
		log.Warn("webhook delivery failed, will retry", "err", err, "retry_at", retryAt)
		w.finishDelivery(d, w.o.Deliveries.RetryDelivery(fctx, d, err.Error(), retryAt))
	}
}

func (w *Worker) finishDelivery(d store.Delivery, err error) {
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		// 处理超过租约，投递已被其他实例重新领取：结果以对方为准
		w.log.Warn("webhook delivery lease lost", "delivery", d.DeliveryID, "attempt", d.Attempts)
	case err != nil:
		w.log.Error("webhook delivery update failed", "delivery", d.DeliveryID, "err", err)
	}
}

func (w *Worker) processDelivery(ctx context.Context, d store.Delivery) error {
	if d.Kind == webhook.Invalid {
		return permanent(fmt.Errorf("unparsable payload: %s", d.ParseError))
	}
	bot := w.o.BotLogin()
	if bot == "" && needsBot(d.Kind) {
		return &ticket.DeferError{Until: w.o.Now().Add(botRetry), Reason: "bot login not yet known"}
	}
	// 按当前的机器人账号重新解析：接收时的 FromBot 可能是在账号尚未获取时判定的
	ev, err := webhook.Parse(&gitea.Delivery{ID: d.DeliveryID, Event: d.Event, EventType: d.EventType, Body: d.Payload,
		ReceivedAt: d.ReceivedAt}, webhook.ParseOptions{BotLogin: bot, ReplyMode: w.o.ReplyMode})
	if err != nil {
		return permanent(err)
	}
	switch ev.Kind {
	case webhook.IssueOpened, webhook.IssueEdited, webhook.IssueClosed, webhook.IssueReopened, webhook.IssueDeleted,
		webhook.IssueAssigned, webhook.IssueUnassigned, webhook.IssueLabeled, webhook.IssueMilestoned, webhook.IssueDemilestoned:
		return w.applyIssue(ctx, ev, d.ReceivedAt)
	case webhook.CommentCreated, webhook.CommentEdited, webhook.CommentDeleted:
		return w.applyComment(ctx, ev, d.ReceivedAt)
	case webhook.PullOpened, webhook.PullEdited, webhook.PullReopened, webhook.PullClosed, webhook.PullMerged:
		return w.applyPull(ctx, ev)
	}
	return w.applyRelease(ctx, ev, d.ReceivedAt)
}

// needsBot 报告处理该类事件是否依赖机器人账号：评论要识别机器人自己发的，opened 要采信机器人建单的标记。
func needsBot(k webhook.Kind) bool {
	switch k {
	case webhook.CommentCreated, webhook.CommentEdited, webhook.CommentDeleted, webhook.IssueOpened:
		return true
	}
	return false
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (w *Worker) applyIssue(ctx context.Context, ev *webhook.Event, received time.Time) error {
	change := ticket.IssueChange(ev.Kind)
	if ev.MarkerTicketID == 0 || change != ticket.IssueOpened {
		ok, err := w.o.Tickets.Tracks(ctx, ev.Repo, ev.IssueNumber)
		if err != nil || !ok {
			return err // 不是灵问工单的 Issue
		}
	}
	cur := snapshot(ev.Issue)
	switch change {
	case ticket.IssueOpened, ticket.IssueEdited, ticket.IssueDeleted:
	default:
		// 状态类事件只是触发器：以 Gitea 当前状态为准
		owner, name, _ := strings.Cut(ev.Repo, "/")
		is, err := w.o.Gitea.GetIssue(ctx, owner, name, ev.IssueNumber)
		switch {
		case errors.Is(err, gitea.ErrNotFound):
			// Issue 已删除或仓库不可访问；载荷里的旧快照不再可信
			change, cur = ticket.IssueDeleted, ticket.IssueState{}
		case err != nil && gitea.IsRetryable(err):
			return fmt.Errorf("get issue %s#%d: %w", ev.Repo, ev.IssueNumber, err)
		case err != nil:
			w.log.Warn("get issue failed, using webhook payload", "repo", ev.Repo, "issue", ev.IssueNumber, "err", err)
		default:
			cur = snapshot(is)
		}
	}
	return w.o.Tickets.ApplyIssue(ctx, ticket.IssueUpdate{Change: change, Repo: ev.Repo, Number: ev.IssueNumber,
		MarkerTicketID: ev.MarkerTicketID, Sender: ev.Sender, DeliveryID: ev.DeliveryID, ReceivedAt: received, Current: cur})
}

func snapshot(is *gitea.Issue) ticket.IssueState {
	if is == nil {
		return ticket.IssueState{}
	}
	s := ticket.IssueState{State: is.State, DueDate: is.DueDate, UpdatedAt: is.UpdatedAt}
	for _, l := range is.Labels {
		s.Labels = append(s.Labels, l.Name)
	}
	for _, u := range is.Assignees {
		s.Assignees = append(s.Assignees, u.Login)
	}
	if is.Milestone != nil {
		s.Milestone = is.Milestone.Title
		if s.DueDate == nil {
			s.DueDate = is.Milestone.DueOn
		}
	}
	return s
}

func (w *Worker) applyComment(ctx context.Context, ev *webhook.Event, received time.Time) error {
	if ev.IsPull {
		return nil // PR 上的评论不属于工单 Issue
	}
	c := ev.Comment
	u := ticket.CommentUpdate{Change: ticket.CommentChange(ev.Kind), Repo: ev.Repo, Number: ev.IssueNumber, CommentID: c.ID,
		Author: ev.Sender, Body: c.Body, Public: ev.Public, FromBot: ev.FromBot, DeliveryID: ev.DeliveryID, At: c.UpdatedAt}
	if c.User != nil {
		u.Author = c.User.Login
		// 研发编辑或删除机器人发的评论（灵问转发的补充）：评论属于机器人，不当作研发回复
		if bot := w.o.BotLogin(); bot != "" && strings.EqualFold(c.User.Login, bot) {
			u.FromBot = true
		}
	}
	if u.Public {
		u.Body = gitea.PublicReplyText(c.Body)
	}
	if u.Change == ticket.CommentDeleted {
		u.At = received // 载荷中只有评论最后编辑的时间
	}
	return w.o.Tickets.ApplyComment(ctx, u)
}

func (w *Worker) applyPull(ctx context.Context, ev *webhook.Event) error {
	u := ticket.PullUpdate{Change: ticket.PullChange(ev.Kind), Repo: ev.Repo, Number: ev.IssueNumber, Sender: ev.Sender,
		DeliveryID: ev.DeliveryID, MergeCommit: ev.PullRequest.MergeCommitSHA}
	// "#5" 与 "owner/repo#5" 可能指同一 Issue（仓库名不区分大小写）：补全仓库后合并，任一带关闭关键词即算关闭
	seen := map[string]int{}
	for _, r := range ev.Refs {
		repo := ev.Repo
		if r.Owner != "" {
			repo = r.Owner + "/" + r.Repo
		}
		k := strings.ToLower(repo) + "#" + strconv.FormatInt(r.Index, 10)
		closes := r.Action == gitea.RefCloses
		if i, ok := seen[k]; ok {
			u.Refs[i].Closes = u.Refs[i].Closes || closes
			continue
		}
		seen[k] = len(u.Refs)
		u.Refs = append(u.Refs, ticket.IssueRef{Repo: repo, Number: r.Index, Closes: closes})
	}
	if len(u.Refs) == 0 {
		return nil
	}
	return w.o.Tickets.ApplyPull(ctx, u)
}
