package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/release"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// Cursors 保存各仓库的对账游标，由 store.Store 实现。
type Cursors interface {
	Cursor(ctx context.Context, repo, kind string) (time.Time, bool, error)
	SetCursor(ctx context.Context, repo, kind string, at time.Time) error
}

// ReconcileReader 是对账用到的 Gitea 读接口，由 gitea.Client 实现。
type ReconcileReader interface {
	ListIssues(ctx context.Context, owner, repo string, o gitea.ListIssuesOptions) ([]*gitea.Issue, *gitea.Response, error)
	GetPullRequest(ctx context.Context, owner, repo string, index int64) (*gitea.PullRequest, error)
	ListRepoComments(ctx context.Context, owner, repo string, since time.Time, o gitea.ListOptions) ([]*gitea.Comment, *gitea.Response, error)
	ListReleases(ctx context.Context, owner, repo string, o gitea.ListReleasesOptions) ([]*gitea.Release, *gitea.Response, error)
}

// ReconcileOptions 配置定时对账：Gitea 投递失败不会自动重试，按游标增量拉取补齐（PRD 3.2.2）。
type ReconcileOptions struct {
	Interval time.Duration // ≤0 不对账
	Repos    []string      // 对接仓库 "owner/name"
	Cursors  Cursors
	Gitea    ReconcileReader
	// Lock 在多实例间互斥一次对账；拿不到锁时跳过本次。为空时不加锁。
	Lock func(ctx context.Context) (release func(), ok bool, err error)
}

const (
	// reconcileOverlap 是游标的回看时间：Gitea 的 since 按更新时间过滤，回看一段避免边界与时钟偏差漏数据（处理幂等）。
	reconcileOverlap = 2 * time.Minute
	// reconcileFirst 是从未对账过的仓库回看的时长。
	reconcileFirst = 24 * time.Hour
)

// errSkip 表示本类这次跳过（如机器人账号尚未获取），不前移游标、不算失败。
var errSkip = errors.New("skipped")

// Reconcile 按 PR → Issue → 评论 → 版本的顺序增量对账一次，每一类先处理完全部仓库再进入下一类：跨仓库修复
// （前端 PR 关闭后端 Issue）的合并也先于 Issue 关闭记录。某类失败时记日志、不前移该类游标，下次重试；
// 各处理幂等，与 Webhook 重叠不会重复通知或重复留痕。
func (w *Worker) Reconcile(ctx context.Context) error {
	ro := w.o.Reconcile
	if ro.Gitea == nil || ro.Cursors == nil || !w.ready() {
		return nil
	}
	if ro.Lock != nil {
		release, ok, err := ro.Lock(ctx)
		if err != nil || !ok {
			return err // 其他实例正在对账
		}
		defer release()
	}
	var errs []error
	for _, k := range []struct {
		kind string
		fn   func(ctx context.Context, repo, owner, name string, since time.Time) error
	}{{"pulls", w.reconcilePulls}, {"issues", w.reconcileIssues}, {"comments", w.reconcileComments}, {"releases", w.reconcileReleases}} {
		for _, repo := range ro.Repos {
			if ctx.Err() != nil {
				return errors.Join(errs...)
			}
			err := w.reconcileKind(ctx, repo, k.kind, k.fn)
			switch {
			case errors.Is(err, errSkip):
				w.log.Debug("reconcile skipped", "repo", repo, "kind", k.kind, "err", err)
			case err != nil:
				w.log.Warn("reconcile failed, will retry next run", "repo", repo, "kind", k.kind, "err", err)
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) reconcileKind(ctx context.Context, repo, kind string, fn func(ctx context.Context, repo, owner, name string, since time.Time) error) error {
	cur, ok, err := w.o.Reconcile.Cursors.Cursor(ctx, repo, kind)
	if err != nil {
		return err
	}
	start := w.o.Now()
	since := cur.Add(-reconcileOverlap)
	if !ok {
		since = start.Add(-reconcileFirst)
	}
	owner, name, _ := strings.Cut(repo, "/")
	if err := fn(ctx, repo, owner, name, since); err != nil {
		return fmt.Errorf("%s %s: %w", repo, kind, err)
	}
	return w.o.Reconcile.Cursors.SetCursor(ctx, repo, kind, start)
}

func (w *Worker) reconcilePulls(ctx context.Context, repo, owner, name string, since time.Time) error {
	prs, err := gitea.ListAll(ctx, 50, func(ctx context.Context, o gitea.ListOptions) ([]*gitea.Issue, *gitea.Response, error) {
		return w.o.Reconcile.Gitea.ListIssues(ctx, owner, name, gitea.ListIssuesOptions{ListOptions: o, State: gitea.StateAll, Since: since, PullRequests: true})
	})
	if err != nil {
		return err
	}
	for _, is := range prs {
		merged := is.PullRequest != nil && is.PullRequest.Merged
		if !merged && is.State != gitea.StateOpen {
			continue // 未合并就关闭的 PR 不影响工单
		}
		u := ticket.PullUpdate{Repo: repo, Number: is.Number, DeliveryID: "reconcile", Change: ticket.PullOpened, Replay: true}
		for _, r := range resolveRefs(repo, gitea.ParseIssueRefs(is.Title+"\n"+is.Body)) {
			u.Refs = append(u.Refs, ticket.IssueRef{Repo: r.Repo, Number: r.Number, Closes: r.Closes})
		}
		if len(u.Refs) == 0 {
			continue // 列表里已有标题与正文：不引用 Issue 的 PR 不再读详情
		}
		if merged {
			pr, err := w.o.Reconcile.Gitea.GetPullRequest(ctx, owner, name, is.Number)
			if err != nil {
				return err
			}
			u.Change, u.MergeCommit = ticket.PullMerged, pr.MergeCommitSHA
		}
		if err := w.o.Tickets.ApplyPull(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) reconcileIssues(ctx context.Context, repo, owner, name string, since time.Time) error {
	// 不按来源标签过滤：研发可能去掉了标签；since 已限定范围，是否为工单 Issue 以库中关联为准
	issues, err := gitea.ListAll(ctx, 50, func(ctx context.Context, o gitea.ListOptions) ([]*gitea.Issue, *gitea.Response, error) {
		return w.o.Reconcile.Gitea.ListIssues(ctx, owner, name, gitea.ListIssuesOptions{ListOptions: o, State: gitea.StateAll, Since: since})
	})
	if err != nil {
		return err
	}
	var deferred error
	for _, is := range issues {
		ok, err := w.o.Tickets.Tracks(ctx, repo, is.Number)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		// ReceivedAt 取零值：快照已是事后状态，不再等 PR 事件；队列中仍有合并投递时照常推迟
		err = w.o.Tickets.ApplyIssue(ctx, ticket.IssueUpdate{Change: ticket.IssueSynced, Repo: repo, Number: is.Number,
			DeliveryID: "reconcile", Current: snapshot(is)})
		var de *ticket.DeferError
		switch {
		case errors.As(err, &de):
			deferred = err // 有 PR 合并事件仍在队列中：本类游标不前移，下次再对
		case err != nil:
			return err
		}
	}
	return deferred
}

func (w *Worker) reconcileComments(ctx context.Context, repo, owner, name string, since time.Time) error {
	if w.o.BotLogin() == "" {
		return fmt.Errorf("%w: bot login not yet known", errSkip) // 无法识别机器人自己的评论，下次再对
	}
	cs, err := gitea.ListAll(ctx, 50, func(ctx context.Context, o gitea.ListOptions) ([]*gitea.Comment, *gitea.Response, error) {
		return w.o.Reconcile.Gitea.ListRepoComments(ctx, owner, name, since, o)
	})
	if err != nil {
		return err
	}
	for _, c := range cs {
		number := gitea.IssueIndexFromURL(c.IssueURL)
		if number == 0 {
			continue
		}
		ok, err := w.o.Tickets.Tracks(ctx, repo, number)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		u := w.commentUpdate(ticket.CommentEdited, repo, number, c, "")
		u.DeliveryID = "reconcile"
		if err := w.o.Tickets.ApplyComment(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) reconcileReleases(ctx context.Context, repo, owner, name string, since time.Time) error {
	if w.o.Releases == nil {
		return nil
	}
	// Release 没有 updated_at：按创建时间倒序翻页，翻到整页都早于 since 为止，再按发布时间筛选（处理幂等）
	var rs []*gitea.Release
	for page := 1; ; page++ {
		batch, resp, err := w.o.Reconcile.Gitea.ListReleases(ctx, owner, name, gitea.ListReleasesOptions{ListOptions: gitea.ListOptions{Page: page, Limit: 50}})
		if err != nil {
			return err
		}
		recent := false
		for _, r := range batch {
			if r.PublishedAt == nil || !r.PublishedAt.Before(since) || !r.CreatedAt.Before(since) {
				recent = true
			}
		}
		rs = append(rs, batch...)
		if !recent || len(batch) == 0 || resp == nil || resp.NextPage == 0 {
			break
		}
	}
	for i := len(rs) - 1; i >= 0; i-- { // 先旧后新
		r := rs[i]
		if r.Draft || r.PublishedAt == nil || r.PublishedAt.Before(since) {
			continue
		}
		err := w.o.Releases.Published(ctx, release.Release{Repo: repo, Tag: r.TagName, Name: r.Name, Body: r.Body,
			Prerelease: r.Prerelease, PublishedAt: *r.PublishedAt, GiteaID: r.ID, URL: r.HTMLURL}, releaseRefs(repo, r.Body), false)
		if err != nil && !errors.Is(err, release.ErrNotFound) {
			return err
		}
	}
	return nil
}
