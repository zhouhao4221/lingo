package lingo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/project"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
	"github.com/zhouhao4221/lingo/internal/webhook"
	"github.com/zhouhao4221/lingo/internal/worker"
)

// Config 是 lingo 的全部配置，由接入方在代码中构造。
type Config struct {
	DB           *sql.DB // PostgreSQL，驱动由接入方选择
	Model        Model
	Gitea        GiteaConfig
	Projects     []Project // 第一个为默认项目（请求未指定 project 时使用）
	Authenticate Authenticator
	Notifier     Notifier // 可空，为空时不回传进展
	Tools        []Tool
	// MaxAttachmentBytes 是单个附件大小上限，0 表示默认 10 MB。
	MaxAttachmentBytes int64
	// TriageRemindAfter 是待确认工单的提醒间隔：超过该时长未处理发出 triage.overdue，之后按同样间隔重复；
	// 0 表示默认 24 小时，负数关闭提醒（接入方可用 ListTriage 的 PendingBefore 自行实现）。
	TriageRemindAfter time.Duration
	// Logger 为空时使用 slog.Default()。
	Logger *slog.Logger
}

// Lingo 是一个灵问实例。
type Lingo struct {
	cfg     Config
	log     *slog.Logger
	gitea   *gitea.Client
	store   *store.Store
	webhook http.Handler
	rest    http.Handler
	worker  *worker.Worker
	tickets *ticket.Service

	projectsMu     sync.Mutex
	projectsSynced bool // Config.Projects 已同步入库

	botMu       sync.Mutex
	bot         string    // 机器人账号 login，首次成功获取后缓存
	botFetching bool      // 后台获取进行中
	botTried    time.Time // 最近一次发起获取的时间，失败后每分钟最多重试一次
}

// New 校验配置并创建实例；不执行数据库迁移。
func New(cfg Config) (*Lingo, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("lingo: Config.DB is required")
	case cfg.Model == nil:
		return nil, errors.New("lingo: Config.Model is required")
	case cfg.Gitea.BaseURL == "" || cfg.Gitea.Token == "":
		return nil, errors.New("lingo: Config.Gitea.BaseURL and Token are required")
	case cfg.Authenticate == nil:
		return nil, errors.New("lingo: Config.Authenticate is required")
	case len(cfg.Projects) == 0:
		return nil, errors.New("lingo: at least one Project is required")
	case cfg.Gitea.AutoRegisterWebhook && cfg.Gitea.WebhookURL == "":
		return nil, errors.New("lingo: Config.Gitea.WebhookURL is required when AutoRegisterWebhook is set")
	case cfg.Gitea.AutoRegisterWebhook && cfg.Gitea.WebhookSecret == "":
		return nil, errors.New("lingo: Config.Gitea.WebhookSecret is required when AutoRegisterWebhook is set")
	}
	if err := project.Validate(projectSpecs(cfg.Projects)); err != nil {
		return nil, fmt.Errorf("lingo: Config.Projects: %w", err)
	}
	l := &Lingo{cfg: cfg, log: cfg.Logger}
	if l.log == nil {
		l.log = slog.Default()
	}
	var err error
	if l.gitea, err = gitea.New(gitea.Options{BaseURL: cfg.Gitea.BaseURL, Token: cfg.Gitea.Token}); err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	var opts []store.Option
	if cfg.Notifier != nil {
		opts = append(opts, store.WithNotify())
	}
	if l.store, err = store.New(cfg.DB, opts...); err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	if l.webhook, err = l.newWebhookHandler(); err != nil {
		return nil, err
	}
	if l.tickets, err = ticket.NewService(ticket.ServiceOptions{
		Store:       l.store,
		AutoForward: l.defaultAutoForward,
		IssueURL:    l.issueURL,
		Declared:    l.declared,
	}); err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	if l.worker, err = l.newWorker(); err != nil {
		return nil, err
	}
	l.rest = l.newRESTHandler()
	l.botLogin() // 立即在后台获取机器人账号，缩短接收端无法识别机器人事件的窗口
	return l, nil
}

func (l *Lingo) replyMode() webhook.ReplyMode {
	if l.cfg.Gitea.ReplyMode == ReplyAll {
		return webhook.ReplyAll
	}
	return webhook.ReplyMarked
}

func (l *Lingo) labelPrefix() string {
	if l.cfg.Gitea.LabelPrefix != "" {
		return l.cfg.Gitea.LabelPrefix
	}
	return "lingo"
}

// remindEvery 是待确认提醒间隔：0 取默认 24 小时，负数关闭。
func (l *Lingo) remindEvery() time.Duration {
	switch d := l.cfg.TriageRemindAfter; {
	case d == 0:
		return 24 * time.Hour
	case d < 0:
		return 0
	default:
		return d
	}
}

func (l *Lingo) issueURL(repo string, n int64) string {
	return ticket.IssueURL(l.cfg.Gitea.BaseURL, repo, n)
}

func (l *Lingo) newWorker() (*worker.Worker, error) {
	tickets, err := ticket.NewSync(ticket.SyncOptions{
		Store:         l.store,
		NeedInfoLabel: l.labelPrefix() + "/review/need-info",
		PendingMerges: l.store.HasPendingMerges,
		IssueURL:      l.issueURL,
	})
	if err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	o := worker.Options{
		Deliveries: l.store,
		Gitea:      l.gitea,
		Tickets:    tickets,
		BotLogin:   l.botLogin,
		ReplyMode:  l.replyMode(),
		Logger:     l.log,

		Tasks:       l.store,
		Issues:      l.gitea,
		Drafts:      l.tickets,
		Repos:       l.repoConfig,
		LabelPrefix: l.labelPrefix(),

		Reminder:    l.tickets,
		RemindEvery: l.remindEvery(),
	}
	if n := l.cfg.Notifier; n != nil {
		o.Notifications = l.store
		o.Notify = func(ctx context.Context, e notify.Event) error { return n.Notify(ctx, publicEvent(e)) }
	}
	w, err := worker.New(o)
	if err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	return w, nil
}

func publicEvent(e notify.Event) Event {
	return Event{ID: e.ID, Kind: EventKind(e.Kind), At: e.At, ProjectKey: e.ProjectKey, TicketID: e.TicketID,
		TicketType: TicketType(e.TicketType), Title: e.Title, Status: e.Status, PrevStatus: e.PrevStatus,
		Action: TriageAction(e.Action), UserID: e.UserID, ActorID: e.ActorID, Body: e.Body, URL: e.URL}
}

func (l *Lingo) newWebhookHandler() (http.Handler, error) {
	if l.cfg.Gitea.WebhookSecret == "" {
		// 未配置密钥时拒绝所有投递：空密钥的签名任何人都能伪造
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			l.log.Error("lingo: webhook received but Config.Gitea.WebhookSecret is empty")
			http.Error(w, "webhook secret not configured", http.StatusServiceUnavailable)
		}), nil
	}
	h, err := webhook.NewHandler(webhook.Options{
		Secret:    l.cfg.Gitea.WebhookSecret,
		BotLogin:  l.botLogin,
		ReplyMode: l.replyMode(),
		Store:     l.store,
		Logger:    l.log,
	})
	if err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	return h, nil
}

// botLogin 返回机器人账号 login。尚未获取时在后台发起获取（同一时间只有一个，失败后每分钟最多重试一次）
// 并立即返回空串，不阻塞 Webhook 请求；Handler 与 Run 分进程部署时同样可用。
// 接收时未识别的机器人事件，由 worker 按当时的账号重新解析判定。
func (l *Lingo) botLogin() string {
	l.botMu.Lock()
	defer l.botMu.Unlock()
	if l.bot == "" && !l.botFetching && time.Since(l.botTried) >= time.Minute {
		l.botFetching, l.botTried = true, time.Now()
		go l.fetchBotLogin()
	}
	return l.bot
}

func (l *Lingo) fetchBotLogin() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := l.gitea.GetCurrentUser(ctx)
	l.botMu.Lock()
	defer l.botMu.Unlock()
	l.botFetching = false
	if err != nil {
		l.log.Warn("lingo: fetch Gitea bot account failed, will retry", "err", err)
		return
	}
	l.bot = u.Login
}

// MCPHandler 返回 MCP Server（Streamable HTTP），工具与 Handler 能力对齐。
func (l *Lingo) MCPHandler() http.Handler { return notImplemented() }

// WebhookHandler 接收 Gitea Webhook（Issue、评论、PR、Release、提交状态、Actions 运行）：验签、去重后落库
// 并立即返回，由 Run 中的 worker 处理。未配置 Config.Gitea.WebhookSecret 时拒绝所有投递（503）。
func (l *Lingo) WebhookHandler() http.Handler { return l.webhook }

// MarkDeployed 告知灵问仓库 repo（"owner/name"）的版本 tag 已在环境 env 上线，
// 关联工单变为已上线并通知提交人；用于部署流水线不经 Gitea Actions / 提交状态上报的场景。
func (l *Lingo) MarkDeployed(ctx context.Context, repo, tag, env string) error {
	return errNotImplemented
}

// Run 运行后台任务直到 ctx 取消，取消后处理完在途任务再返回；多实例运行安全。
// 目前包括：处理 WebhookHandler 落库的投递（同步工单状态、评论，写事件）、把事件投递给 Config.Notifier。
// Gitea 初始化与对账补偿、知识库生成待实现。
func (l *Lingo) Run(ctx context.Context) error {
	l.botLogin() // 机器人账号获取由 botLogin 统一负责（New 时已发起）
	if err := l.ensureProjects(ctx); err != nil {
		// 不阻断启动：各方法调用时会再次尝试同步
		l.log.Error("lingo: sync projects failed", "err", err)
	}
	return l.worker.Run(ctx)
}

// ErrNotFound 表示资源不存在或无权访问（两者不区分，避免暴露数据是否存在）。
var ErrNotFound = errors.New("lingo: not found")

var errNotImplemented = errors.New("lingo: not implemented")

// TODO: 骨架阶段，接口待实现。
func notImplemented() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "lingo: not implemented", http.StatusNotImplemented)
	})
}
