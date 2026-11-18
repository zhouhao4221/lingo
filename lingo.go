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
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/webhook"
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
	l := &Lingo{cfg: cfg, log: cfg.Logger}
	if l.log == nil {
		l.log = slog.Default()
	}
	var err error
	if l.gitea, err = gitea.New(gitea.Options{BaseURL: cfg.Gitea.BaseURL, Token: cfg.Gitea.Token}); err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	if l.store, err = store.New(cfg.DB); err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	if l.webhook, err = l.newWebhookHandler(); err != nil {
		return nil, err
	}
	l.botLogin() // 立即在后台获取机器人账号，缩短接收端无法识别机器人事件的窗口
	return l, nil
}

func (l *Lingo) newWebhookHandler() (http.Handler, error) {
	if l.cfg.Gitea.WebhookSecret == "" {
		// 未配置密钥时拒绝所有投递：空密钥的签名任何人都能伪造
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			l.log.Error("lingo: webhook received but Config.Gitea.WebhookSecret is empty")
			http.Error(w, "webhook secret not configured", http.StatusServiceUnavailable)
		}), nil
	}
	mode := webhook.ReplyMarked
	if l.cfg.Gitea.ReplyMode == ReplyAll {
		mode = webhook.ReplyAll
	}
	h, err := webhook.NewHandler(webhook.Options{
		Secret:    l.cfg.Gitea.WebhookSecret,
		BotLogin:  l.botLogin,
		ReplyMode: mode,
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

// Handler 返回 REST JSON 接口（提问、工单、评审、知识库、版本），路径为相对路径，
// 由接入方挂载到任意前缀（配合 http.StripPrefix）。
func (l *Lingo) Handler() http.Handler { return notImplemented() }

// MCPHandler 返回 MCP Server（Streamable HTTP），工具与 Handler 能力对齐。
func (l *Lingo) MCPHandler() http.Handler { return notImplemented() }

// WebhookHandler 接收 Gitea Webhook（Issue、评论、PR、Release、提交状态、Actions 运行）：验签、去重后落库
// 并立即返回，由 Run 中的 worker 处理。未配置 Config.Gitea.WebhookSecret 时拒绝所有投递（503）。
func (l *Lingo) WebhookHandler() http.Handler { return l.webhook }

// SetAutoForward 在运行时覆盖项目 project 下工单类型 t 是否自动转出（仅 admin），存入数据库并优先于
// Project.AutoForward；u 为操作人，用于权限校验与留痕。
func (l *Lingo) SetAutoForward(ctx context.Context, u User, project string, t TicketType, on bool) error {
	return errNotImplemented
}

// ResetAutoForward 删除运行时覆盖，恢复为 Project.AutoForward 的默认值（仅 admin）。
func (l *Lingo) ResetAutoForward(ctx context.Context, u User, project string, t TicketType) error {
	return errNotImplemented
}

// GetTicket 返回工单 id；无权访问时与不存在一样返回 ErrNotFound（见数据隔离）。
func (l *Lingo) GetTicket(ctx context.Context, u User, id int64) (Ticket, error) {
	return Ticket{}, errNotImplemented
}

// ListTriage 列出 u 可见项目中的待确认工单（仅 reviewer / admin）。
func (l *Lingo) ListTriage(ctx context.Context, u User, f TriageFilter) ([]Ticket, error) {
	return nil, errNotImplemented
}

// TriageSummary 返回 u 可见项目的待确认数与超时数（仅 reviewer / admin）。
func (l *Lingo) TriageSummary(ctx context.Context, u User) ([]TriageCount, error) {
	return nil, errNotImplemented
}

// ListEvents 返回 ID 大于 afterID 的事件（按 ID 升序，最多 limit 条，0 表示默认 100），
// 与 Notifier 推送的事件同源；按 u 的角色与项目过滤。
func (l *Lingo) ListEvents(ctx context.Context, u User, afterID int64, limit int) ([]Event, error) {
	return nil, errNotImplemented
}

// MarkDeployed 告知灵问仓库 repo（"owner/name"）的版本 tag 已在环境 env 上线，
// 关联工单变为已上线并通知提交人；用于部署流水线不经 Gitea Actions / 提交状态上报的场景。
func (l *Lingo) MarkDeployed(ctx context.Context, repo, tag, env string) error {
	return errNotImplemented
}

// Run 运行后台任务（outbox 投递、Gitea 初始化与对账补偿、知识库生成），直到 ctx 取消；多实例运行安全。
func (l *Lingo) Run(ctx context.Context) error {
	l.botLogin() // 机器人账号获取由 botLogin 统一负责（New 时已发起）
	<-ctx.Done()
	return nil
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
