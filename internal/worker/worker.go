package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/llm"
	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
	"github.com/zhouhao4221/lingo/internal/webhook"
)

// DeliveryStore 是待处理 Webhook 投递的队列，由 store.Store 实现。
type DeliveryStore interface {
	ClaimDeliveries(ctx context.Context, limit int, lease time.Duration) ([]store.Delivery, error)
	CompleteDelivery(ctx context.Context, d store.Delivery) error
	RetryDelivery(ctx context.Context, d store.Delivery, reason string, retryAt time.Time) error
	ReleaseDelivery(ctx context.Context, d store.Delivery, reason string, retryAt time.Time) error
	AbandonDelivery(ctx context.Context, d store.Delivery, reason string) error
}

// NotificationStore 是待投递通知的队列，由 store.Store 实现。
type NotificationStore interface {
	ClaimNotifications(ctx context.Context, limit int, lease time.Duration) ([]store.Notification, error)
	CompleteNotification(ctx context.Context, n store.Notification) error
	RetryNotification(ctx context.Context, n store.Notification, reason string, retryAt time.Time) error
	ReleaseNotification(ctx context.Context, n store.Notification, reason string, retryAt time.Time) error
	FailNotification(ctx context.Context, n store.Notification, reason string) error
}

// IssueReader 读取 Issue 当前状态，由 gitea.Client 实现。
type IssueReader interface {
	GetIssue(ctx context.Context, owner, repo string, index int64) (*gitea.Issue, error)
}

// Tickets 把 Gitea 变化同步到工单，由 ticket.Sync 实现。
type Tickets interface {
	Tracks(ctx context.Context, repo string, number int64) (bool, error)
	ApplyIssue(ctx context.Context, u ticket.IssueUpdate) error
	ApplyComment(ctx context.Context, u ticket.CommentUpdate) error
	ApplyPull(ctx context.Context, u ticket.PullUpdate) error
}

// Options 是 Worker 配置。
type Options struct {
	Deliveries DeliveryStore // 必填
	Gitea      IssueReader   // 必填
	Tickets    Tickets       // 必填
	// BotLogin 返回机器人账号 login，未就绪时返回空串（依赖它判定的投递会推迟处理）。
	BotLogin  func() string
	ReplyMode webhook.ReplyMode

	// Notifications 与 Notify 同时非空时运行通知投递循环。
	Notifications NotificationStore
	Notify        func(ctx context.Context, e notify.Event) error

	// Tasks、Issues、Drafts 同时非空时运行 Gitea 写操作循环（建 Issue 等）。
	Tasks      TaskStore
	Issues     IssueWriter
	Drafts     Drafts
	Forwarding Forwarding
	// Drafter 非空时建 Issue 前用轻量模型整理工单内容（失败时按原内容建单）。
	Drafter     Drafter
	Repos       func(repo string) RepoConfig // 仓库建单配置；为空时不指派、不指定模板
	LabelPrefix string                       // 默认 "lingo"
	Templates   *gitea.TemplateCache         // 可与其他组件共用；为空时自建

	// Releases 非空时处理 Release、tag、提交状态与 Actions 运行事件；Repos 提供各仓库的上线信号配置。
	Releases Releases
	Git      GitReader // Releases 非空时必填

	// Reconcile 配置定时对账（Interval > 0 时运行）。
	Reconcile ReconcileOptions

	// Reminder 非空时每分钟清理孤儿附件；RemindEvery > 0 时同时检查待确认超时（triage.overdue）。
	Reminder       Reminder
	RemindEvery    time.Duration
	RemindProjects []string // 只提醒这些项目（当前声明的项目）

	Logger       *slog.Logger
	PollInterval time.Duration // 队列为空时的轮询间隔，默认 1 秒
	BatchSize    int           // 每次领取条数，默认 20
	MaxAttempts  int           // 单条最多失败次数（推迟与释放不计），达到后放弃，默认 10
	Lease        time.Duration // 领取租约，默认 store.DefaultLease
	// TaskTimeout 是单条投递或通知的处理时限，默认 1 分钟，最长 Lease 的一半。一批条目共用一个租约，
	// 剩余租约不足以再处理一条时，其余条目释放回队列，避免租约过期后被其他实例重复处理。
	TaskTimeout time.Duration
	Now         func() time.Time
}

// Drafter 整理待建 Issue 的工单内容，由 llm.Drafter 实现。
type Drafter interface {
	DraftIssue(ctx context.Context, in llm.DraftInput) (*llm.Draft, error)
}

// Reminder 发出待确认超时提醒并清理孤儿附件，由 ticket.Service 实现。
type Reminder interface {
	RemindOverdue(ctx context.Context, projects []string, every time.Duration, limit int) (int, error)
	CleanupAttachments(ctx context.Context, maxAge time.Duration) (int, error)
}

// orphanAge 是未关联工单的附件的保留时长。
const orphanAge = 24 * time.Hour

// reconcileStartDelay 是 Run 启动后首次对账前的等待。
const reconcileStartDelay = 30 * time.Second

// remindInterval 是检查待确认超时的间隔。
const remindInterval = time.Minute

// Worker 消费 Webhook 投递与通知 outbox；多实例运行安全（领取用 SKIP LOCKED + 租约）。
type Worker struct {
	o         Options
	log       *slog.Logger
	labels    labelCache
	templates *gitea.TemplateCache
	drafts    draftCache
}

// New 校验配置并创建 Worker。
func New(o Options) (*Worker, error) {
	switch {
	case o.Deliveries == nil:
		return nil, errors.New("worker: Deliveries is required")
	case o.Gitea == nil:
		return nil, errors.New("worker: Gitea is required")
	case o.Tickets == nil:
		return nil, errors.New("worker: Tickets is required")
	}
	if o.BotLogin == nil {
		o.BotLogin = func() string { return "" }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 20
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 10
	}
	if o.Lease <= 0 {
		o.Lease = store.DefaultLease
	}
	if o.TaskTimeout <= 0 {
		o.TaskTimeout = time.Minute
	}
	o.TaskTimeout = min(o.TaskTimeout, o.Lease/2)
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Repos == nil {
		o.Repos = func(string) RepoConfig { return RepoConfig{} }
	}
	if o.LabelPrefix == "" {
		o.LabelPrefix = "lingo"
	}
	if o.Releases != nil && o.Git == nil {
		return nil, errors.New("worker: Git is required with Releases")
	}
	if o.Tasks != nil && (o.Issues == nil || o.Drafts == nil || o.Forwarding == nil) {
		return nil, errors.New("worker: Issues, Drafts and Forwarding are required with Tasks")
	}
	tc := o.Templates
	if tc == nil {
		tc = &gitea.TemplateCache{}
	}
	return &Worker{o: o, log: o.Logger.With("component", "lingo.worker"), templates: tc}, nil
}

// Run 运行各处理循环直到 ctx 取消；取消后处理完在途的一条再返回，已领取未开始的条目立即释放。
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	loop := func(name string, step func(context.Context) (int, error)) {
		defer wg.Done()
		for {
			n, err := step(ctx)
			if err != nil && ctx.Err() == nil {
				w.log.Error("worker loop failed", "loop", name, "err", err)
			}
			if n > 0 && err == nil {
				continue // 队列可能还有积压，立即领取下一批
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.o.PollInterval):
			}
		}
	}
	wg.Add(1)
	go loop("webhook", w.ProcessWebhooks)
	if w.o.Notifications != nil && w.o.Notify != nil {
		wg.Add(1)
		go loop("notify", w.DeliverNotifications)
	}
	if w.o.Tasks != nil {
		wg.Add(1)
		go loop("gitea", w.ProcessTasks)
	}
	if ro := w.o.Reconcile; ro.Interval > 0 && ro.Gitea != nil && ro.Cursors != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 启动后稍等再对账：机器人账号获取、积压的 Webhook 投递先处理
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(ro.Interval, reconcileStartDelay)):
			}
			t := time.NewTicker(ro.Interval)
			defer t.Stop()
			for {
				if err := w.Reconcile(ctx); err != nil && ctx.Err() == nil {
					w.log.Warn("reconcile run incomplete", "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	if w.o.Reminder != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(remindInterval)
			defer t.Stop()
			limit := min(w.o.BatchSize, ticket.MaxLimit)
			for {
				if _, err := w.o.Reminder.CleanupAttachments(ctx, orphanAge); err != nil && ctx.Err() == nil {
					w.log.Error("worker loop failed", "loop", "cleanup", "err", err)
				}
				for w.o.RemindEvery > 0 {
					n, err := w.o.Reminder.RemindOverdue(ctx, w.o.RemindProjects, w.o.RemindEvery, limit)
					if err != nil && ctx.Err() == nil {
						w.log.Error("worker loop failed", "loop", "remind", "err", err)
					}
					if err != nil || n < limit {
						break
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	wg.Wait()
	return nil
}

// backoff 是第 attempts 次失败后的重试间隔：10 秒起翻倍，最长 1 小时。
func backoff(attempts int) time.Duration {
	d := 10 * time.Second
	for i := 1; i < attempts && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// permanentError 表示重试也不会成功的错误，直接放弃。
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err: err} }

// finishTimeout 是写回处理结果的时限，与任务本身的时限分开，任务耗尽时限后结果仍能保存。
const finishTimeout = 10 * time.Second

// taskContext 让单条任务在 ctx 取消后仍能处理完（优雅退出），但不超过 TaskTimeout。
func (w *Worker) taskContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), w.o.TaskTimeout)
}

// finishContext 用于写回处理结果：不受 ctx 取消与任务超时影响。
func finishContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
}

// batchBudget 返回一批条目在租约内还能开始处理新条目的截止时间；每批的第一条总会处理。
func (w *Worker) batchBudget(claimed time.Time) time.Time {
	return claimed.Add(w.o.Lease - w.o.TaskTimeout - finishTimeout)
}
