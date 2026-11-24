package lingo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/zhouhao4221/lingo/internal/knowledge"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhouhao4221/lingo/internal/assistant"
	"github.com/zhouhao4221/lingo/internal/connector"
	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/llm"
	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/project"
	"github.com/zhouhao4221/lingo/internal/release"
	"github.com/zhouhao4221/lingo/internal/settings"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
	"github.com/zhouhao4221/lingo/internal/webhook"
	"github.com/zhouhao4221/lingo/internal/worker"
)

// Config 是 lingo 的全部配置，由接入方在代码中构造。
type Config struct {
	DB *sql.DB // PostgreSQL，驱动由接入方选择
	// Model 是主模型（对话 + 向量），供知识问答组织回答（实现 StreamModel 时流式输出）；为空时知识问答不可用
	// （SendMessage 返回 ErrNoModel），其他功能不受影响。
	Model Model
	// LightModel 是轻量模型（只用 Chat），用于建 Issue 前整理工单（生成标题、从描述中抽取模板字段、整理描述），以及
	// 知识问答的意图解析与确认判断。为空时用 Model；两者都为空时按原内容建单。整理失败不影响建单，Issue 末尾附提交人原始描述。
	LightModel   Model
	Gitea        GiteaConfig
	Projects     []Project // 第一个为默认项目（请求未指定 project 时使用）
	Authenticate Authenticator
	Notifier     Notifier // 可空，为空时不回传进展
	Tools        []Tool
	// FetchAttachment 读取附件内容（可选）。附件由接入方自己的附件系统保存，灵工只记录引用；配置后转出 Issue 时
	// 经它读取内容上传为 Gitea 附件（图片在 Issue 中直接显示，单个最多 100 MB）；为空时只把附件地址写进 Issue。
	// 灵工不会自己请求附件 URL（避免以服务端身份访问用户填写的地址）。
	//
	// userID 是提交该附件的用户（工单提交人或补充评论的作者）。附件 ID 由调用方传入、灵工不校验归属，接入方必须确认
	// 该附件属于此用户（或其有权访问），否则返回 ErrNotFound / ErrForbidden——该附件不会上传，Issue 中注明未能上传。
	// 其他错误视为临时错误，随任务重试，连续失败 3 次后同样注明。
	FetchAttachment func(ctx context.Context, userID string, a Attachment) (io.ReadCloser, error)
	// RequiredPerspectives 是需求工单默认的必填视角（可被 Project.RequiredPerspectives 覆盖）；为空不检查。
	// 转出时缺少必填视角发出 ticket.perspective_missing，Issue 的视角区块中标注“缺少：…”。
	RequiredPerspectives []Perspective
	// AuthorizePerspective 判断 u 能否补充（write）或查看需求工单 t 的视角补充，Go 方法、REST 与 MCP 都经过它；
	// 返回 nil 允许，返回 ErrNotFound（不暴露工单存在）或 ErrForbidden 拒绝。调用前 lingo 已校验 t 为需求工单且在
	// u.Projects 内。为空时按工单可见范围：external 只能是本人的需求，agent 只能是已转出的，reviewer / admin 为项目内全部。
	// 要让业务方、专家、决策者参与他人的需求，在这里按接入方自己的规则放行。
	AuthorizePerspective func(ctx context.Context, u User, t Ticket, write bool) error
	// TicketAudience 返回 external 用户 u 额外可见哪些提交人的工单（如同部门、同团队成员），在固定角色规则之外放宽；
	// 为空时不放宽。放宽只读且不超出 u.Projects：查看工单与对外评论、ListVisibleTickets、知识问答的查找与汇总、版本
	// 关联的工单跟随放宽；留言、补附件、补充视角、确认留言、附件、ListEvents、Notifier 与需求视角（未配置
	// AuthorizePerspective 时）仍只对本人。只对 external 调用（agent / reviewer / admin 不调用，User.ID 为空也不调用），
	// 每个读请求最多调用一次、不缓存（需要时由接入方缓存）；返回错误时按未放宽处理并记 Warn 日志，请求不失败。
	// 放宽属于隐私变更：启用前应告知用户“同部门成员可以查看你提交的工单”。
	TicketAudience func(ctx context.Context, u User) (TicketAudience, error)
	// AuthorizeSettings 判断 u 能否查看（write 为假）或修改、测试（write 为真）连接配置（ConnectionSettings 等，
	// Go 方法与 REST 都经过它）；返回 nil 允许，ErrForbidden / ErrNotFound 拒绝。为空时只允许 admin。
	AuthorizeSettings func(ctx context.Context, u User, write bool) error
	// TriageRemindAfter 是待确认工单的提醒间隔：超过该时长未处理发出 triage.overdue，之后按同样间隔重复；
	// 0 表示默认 24 小时，负数关闭提醒（接入方可用 ListTriage 的 PendingBefore 自行实现）。
	TriageRemindAfter time.Duration
	// MCPAllowedOrigins 是 MCPHandler 额外允许的浏览器来源（如 "https://app.example.com"）。默认只允许与请求 Host
	// 同源的浏览器请求（防 DNS 重绑定）；前端与 MCP 不同源、或反向代理改写了 Host 时在此列出。非浏览器客户端不受影响。
	MCPAllowedOrigins []string
	// ReconcileInterval 是定时对账的间隔：Gitea 投递 Webhook 失败不会重试，Run 按游标增量拉取各仓库的 PR、Issue、
	// 评论与版本补齐漏掉的变化（多实例时只有一个执行）。0 表示默认 10 分钟，负数关闭。
	ReconcileInterval time.Duration
	// SkipSelfCheck 为真时 Run 启动时不执行自检（也就不会自动注册 Webhook、补齐标签）；可随时调用 SelfCheck。
	SkipSelfCheck bool
	// Logger 为空时使用 slog.Default()。
	Logger *slog.Logger
}

// Lingo 是一个灵工实例。
type Lingo struct {
	cfg     Config
	log     *slog.Logger
	gitea   *gitea.Client
	store   *store.Store
	webhook http.Handler
	rest    http.Handler
	mcp     http.Handler
	worker  *worker.Worker
	settingsState
	connMu sync.Mutex

	tickets   *ticket.Service
	releases  *release.Service
	assistant *assistant.Service
	knowledge *knowledge.Service
	data      *connector.Service // 业务数据工具（REQ-008）
	embedKick chan struct{}      // 知识条目新建 / 编辑后提醒生成向量
	tmpls     *gitea.TemplateCache

	projectsMu     sync.Mutex
	projectsSynced bool // Config.Projects 已同步入库

	botMu       sync.Mutex
	bot         string    // 机器人账号 login，首次成功获取后缓存
	botFetching bool      // 后台获取进行中
	botGen      uint64    // 连接代次：Gitea 地址或令牌变化时递增，旧连接上的获取结果丢弃
	botTried    time.Time // 最近一次发起获取的时间，失败后每分钟最多重试一次
}

// New 校验配置并创建实例；不执行数据库迁移。
func New(cfg Config) (*Lingo, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("lingo: Config.DB is required")
	case cfg.Authenticate == nil:
		return nil, errors.New("lingo: Config.Authenticate is required")
	case len(cfg.Projects) == 0:
		return nil, errors.New("lingo: at least one Project is required")
	}
	if err := project.Validate(projectSpecs(cfg.Projects)); err != nil {
		return nil, fmt.Errorf("lingo: Config.Projects: %w", err)
	}
	if err := validatePerspectives(cfg); err != nil {
		return nil, err
	}
	l := &Lingo{cfg: cfg, log: cfg.Logger, tmpls: &gitea.TemplateCache{}}
	l.settingsChanged = make(chan struct{}, 1)
	if l.log == nil {
		l.log = slog.Default()
	}
	var err error
	// Gitea 地址与令牌可不在代码中配置（存数据库、经 UpdateConnectionSettings 修改）；代码中给了就先校验
	l.gitea = gitea.NewUnconfigured()
	if b := cfg.Gitea.BaseURL; b != "" {
		if u, err := url.Parse(b); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("lingo: Config.Gitea.BaseURL must be an absolute http(s) address")
		}
	}
	if cfg.Gitea.BaseURL != "" && cfg.Gitea.Token != "" {
		if l.gitea, err = gitea.New(giteaOptions(cfg.Gitea)); err != nil {
			return nil, fmt.Errorf("lingo: %w", err)
		}
	}
	var opts []store.Option
	if cfg.Notifier != nil {
		opts = append(opts, store.WithNotify())
	}
	if l.store, err = store.New(cfg.DB, opts...); err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	l.settings = &settings.Service{Store: l.store, Overrides: l.overrides()}
	l.applySettings(settings.Stored{}, true) // 先按代码配置生效
	{
		// 再读一次数据库中保存的配置（未执行迁移 0004 或数据库暂不可用时只告警，按代码配置运行，之后定时重试）
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		l.refreshSettings(ctx)
		cancel()
		l.settingsChecked.Store(time.Now().UnixNano())
	}
	if (cfg.Gitea.BaseURL == "") != (cfg.Gitea.Token == "") && !l.gitea.Configured() {
		// 代码中只配了地址或令牌之一（如环境变量拼错），数据库也没有补齐：按未配置运行，明确提示
		l.log.Warn("lingo: Config.Gitea has only one of BaseURL and Token and the database does not provide the other; running unconfigured")
	}
	l.webhook = l.newWebhookHandler()
	if l.tickets, err = ticket.NewService(ticket.ServiceOptions{
		Store:                l.store,
		AutoForward:          l.defaultAutoForward,
		IssueURL:             l.issueURL,
		Declared:             l.declared,
		NeedInfoLabel:        l.labelPrefix() + "/review/need-info",
		RequiredPerspectives: l.requiredPerspectives,
		AuthorizePerspective: l.authorizePerspective,
	}); err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	if l.releases, err = release.New(release.Options{Store: l.store, Git: worker.Git{R: l.gitea}}); err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	if l.worker, err = l.newWorker(); err != nil {
		return nil, err
	}
	l.embedKick = make(chan struct{}, 1)
	if l.knowledge, err = knowledge.New(knowledge.Options{Store: l.store, Embedder: l.knowledgeEmbedder,
		Extractor: l.knowledgeExtractor, Tickets: l.knowledgeTicket}); err != nil {
		return nil, fmt.Errorf("lingo: %w", err)
	}
	l.data = l.newDataTools(builtinToolNames(l.mcpTools()))
	if l.assistant, err = l.newAssistant(); err != nil {
		return nil, err
	}
	l.rest = l.liveHandler(l.newRESTHandler())
	l.mcp = l.liveHandler(l.newMCPHandler())
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

// resetBot 清除机器人账号缓存（Gitea 地址或令牌变化后重新获取）：代次递增，进行中的旧获取结果作废。
func (l *Lingo) resetBot() {
	l.botMu.Lock()
	defer l.botMu.Unlock()
	l.bot, l.botTried, l.botFetching = "", time.Time{}, false
	l.botGen++
}

// chatOf 把 Model 适配为 llm.Chat；实现了 UsageModel 的模型报告用量。
func chatOf(m Model) llm.Chat {
	return func(ctx context.Context, ms []llm.Message) (string, llm.Usage, error) {
		in := publicMessages(ms)
		if um, ok := m.(UsageModel); ok {
			s, u, err := um.ChatUsage(ctx, in)
			return s, llm.Usage(u), err
		}
		s, err := m.Chat(ctx, in)
		return s, llm.Usage{}, err
	}
}

// streamOf 把实现了 StreamModel 的 Model 适配为 llm.Stream；未实现时为 nil。
func streamOf(m Model) llm.Stream {
	sm, ok := m.(StreamModel)
	if !ok {
		return nil
	}
	return func(ctx context.Context, ms []llm.Message, onDelta func(string)) (string, llm.Usage, error) {
		s, u, err := sm.ChatStream(ctx, publicMessages(ms), onDelta)
		return s, llm.Usage(u), err
	}
}

func publicMessages(ms []llm.Message) []Message {
	out := make([]Message, 0, len(ms))
	for _, x := range ms {
		out = append(out, Message{Role: x.Role, Content: x.Content})
	}
	return out
}

// isNil 报告接口值是否为 nil，含装着 nil 指针的接口（如 var m *MyModel; cfg.LightModel = m）。
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

// reconcileLockKey 是定时对账的 advisory lock 键。
const reconcileLockKey = 0x6c696e676f03

func (l *Lingo) reconcileInterval() time.Duration {
	switch d := l.cfg.ReconcileInterval; {
	case d == 0:
		return 10 * time.Minute
	case d < 0:
		return 0
	default:
		return d
	}
}

func (l *Lingo) repoNames() []string {
	var out []string
	for _, p := range l.cfg.Projects {
		for _, r := range p.Repos {
			out = append(out, r.Owner+"/"+r.Name)
		}
	}
	return out
}

// defaultRemindAfter 是 Config.TriageRemindAfter 为 0 时的提醒间隔。
const defaultRemindAfter = 24 * time.Hour

// remindEvery 是待确认提醒间隔：0 取默认 24 小时，负数关闭（返回 0）。
func (l *Lingo) remindEvery() time.Duration {
	switch d := l.cfg.TriageRemindAfter; {
	case d == 0:
		return defaultRemindAfter
	case d < 0:
		return 0
	default:
		return d
	}
}

// overdueAfter 是 TriageSummary 判定超时的阈值：与提醒间隔一致，关闭提醒时仍按默认 24 小时统计。
func (l *Lingo) overdueAfter() time.Duration {
	if d := l.remindEvery(); d > 0 {
		return d
	}
	return defaultRemindAfter
}

func (l *Lingo) issueURL(repo string, n int64) string {
	base := ""
	if c := l.conn(); c != nil {
		base = c.eff.Gitea.BaseURL
	}
	if base == "" {
		return "" // Gitea 未配置
	}
	return ticket.IssueURL(base, repo, n)
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
		Forwarding:  l.store,
		DrafterFor:  l.drafterFor,
		Ready:       l.gitea.Configured,
		Usage:       l.store,
		Repos:       l.repoConfig,
		LabelPrefix: l.labelPrefix(),
		Templates:   l.tmpls,

		Releases:        l.releases,
		Git:             l.gitea,
		Reminder:        l.tickets,
		FetchAttachment: l.fetchAttachment(),
		RemindEvery:     l.remindEvery(),
		RemindProjects:  l.projectKeys(),
		Reconcile: worker.ReconcileOptions{Interval: l.reconcileInterval(), Repos: l.repoNames(), Cursors: l.store, Gitea: l.gitea,
			Lock: func(ctx context.Context) (func(), bool, error) { return l.store.TryLock(ctx, reconcileLockKey) }},
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
		Action: TriageAction(e.Action), UserID: e.UserID, ActorID: e.ActorID, Body: e.Body, URL: e.URL,
		Perspectives: publicPerspectives(e.Perspectives)}
}

// newWebhookHandler 按当前连接验签：每个请求取当前的处理器（密钥修改后立即按新密钥验签）。
// Gitea 未配置或未设置 Webhook 密钥时拒绝所有投递（503）：空密钥的签名任何人都能伪造。
func (l *Lingo) newWebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.touchSettings()
		c := l.conn()
		if c == nil || c.webhook == nil || !l.gitea.Configured() {
			l.log.Warn("lingo: webhook rejected: Gitea or its webhook secret is not configured")
			http.Error(w, "webhook not configured", http.StatusServiceUnavailable)
			return
		}
		c.webhook.ServeHTTP(w, r)
	})
}

// botLogin 返回机器人账号 login。尚未获取时在后台发起获取（同一时间只有一个，失败后每分钟最多重试一次）
// 并立即返回空串，不阻塞 Webhook 请求；Handler 与 Run 分进程部署时同样可用。
// 接收时未识别的机器人事件，由 worker 按当时的账号重新解析判定。
func (l *Lingo) botLogin() string {
	if !l.gitea.Configured() {
		return "" // Gitea 未配置：不获取
	}
	l.botMu.Lock()
	defer l.botMu.Unlock()
	if l.bot == "" && !l.botFetching && time.Since(l.botTried) >= time.Minute {
		l.botFetching, l.botTried = true, time.Now()
		go l.fetchBotLogin(l.botGen)
	}
	return l.bot
}

func (l *Lingo) fetchBotLogin(gen uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := l.gitea.GetCurrentUser(ctx)
	l.botMu.Lock()
	defer l.botMu.Unlock()
	if gen != l.botGen {
		return // 期间连接已更换：结果属于旧连接，丢弃（新连接的获取由 botLogin 另行发起）
	}
	l.botFetching = false
	if err != nil {
		l.log.Warn("lingo: fetch Gitea bot account failed, will retry", "err", err)
		return
	}
	l.bot = u.Login
}

// WebhookHandler 接收 Gitea Webhook（Issue、评论、PR、Release、提交状态、Actions 运行）：按当前生效的 Webhook 密钥
// （代码配置或数据库中的连接配置）验签、去重后落库并立即返回，由 Run 中的 worker 处理。Gitea 未配置（地址或令牌为空）
// 或未设置 Webhook 密钥时拒绝所有投递（503）。
func (l *Lingo) WebhookHandler() http.Handler { return l.webhook }

// MarkDeployed 告知灵工仓库 repo（"owner/name"）的版本 tag 已在环境 env（为空取 "production"）上线，
// 修复版本不晚于它的工单变为已上线并通知提交人；用于部署流水线不经 Gitea Actions / 提交状态上报的场景。
// 灵工尚未记录该版本（Release 或 tag 未同步）时返回 ErrNotFound。
func (l *Lingo) MarkDeployed(ctx context.Context, repo, tag, env string) error {
	if repo == "" || tag == "" {
		return fmt.Errorf("%w: repo and tag are required", ErrInvalidInput)
	}
	if err := l.ensureProjects(ctx); err != nil {
		return err
	}
	err := l.releases.Deployed(ctx, release.Deployment{Repo: repo, Tag: tag, Env: env, Source: "manual"})
	if errors.Is(err, release.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// Run 运行后台任务直到 ctx 取消，取消后处理完在途任务再返回；多实例运行安全。启动时同步项目，并在后台执行一次
// SelfCheck（见其说明，含 AutoRegisterWebhook 的自动注册）。
// 目前包括：处理 WebhookHandler 落库的投递（同步工单状态、评论，写事件）、把事件投递给 Config.Notifier。
// Gitea 初始化与对账补偿、知识库生成待实现。
func (l *Lingo) Run(ctx context.Context) error {
	l.botLogin() // 机器人账号获取由 botLogin 统一负责（New 时已发起）
	if err := l.ensureProjects(ctx); err != nil {
		// 不阻断启动：各方法调用时会再次尝试同步
		l.log.Error("lingo: sync projects failed", "err", err)
	}
	var checked sync.WaitGroup
	var checking atomic.Bool
	selfCheck := func() { // 不阻断启动；结果写日志，也可调用 SelfCheck 获取；同一时间只跑一次
		if !checking.CompareAndSwap(false, true) {
			l.selfCheckDue.Store(true) // 正在自检：结束后再跑一次（期间配置又变了）
			return
		}
		checked.Add(1)
		go func() {
			defer checked.Done()
			for {
				l.logSelfCheck(ctx)
				checking.Store(false)
				// 自检期间连接配置又变了：再跑一次，确保 Gitea 端拿到最新的密钥与地址
				if ctx.Err() != nil || l.cfg.SkipSelfCheck || !l.selfCheckDue.CompareAndSwap(true, false) || !checking.CompareAndSwap(false, true) {
					return
				}
			}
		}()
	}
	l.selfCheckDue.Store(false) // 启动时的自检已经覆盖
	if !l.cfg.SkipSelfCheck {
		selfCheck()
	}
	checked.Add(1)
	go func() { // 为知识条目生成向量（REQ-006）
		defer checked.Done()
		l.embedKnowledge(ctx)
	}()
	if !l.data.Empty() {
		checked.Add(1)
		go func() { // 业务数据调用审计：中断恢复与 180 天清理（REQ-008）
			defer checked.Done()
			l.maintainDataRequests(ctx)
		}()
	}
	checked.Add(1)
	go func() { // 定时检查连接配置；Gitea 连接变化后补跑一次自检（开启自动注册时同步 Webhook 密钥）
		defer checked.Done()
		l.watchSettings(ctx, selfCheck)
	}()
	err := l.worker.Run(ctx)
	checked.Wait() // 退出前等自检与配置检查结束（它们随 ctx 取消尽快返回）
	l.settingsStopped.Store(true)
	l.settingsWG.Wait() // 以及进行中的后台刷新；之后不再访问数据库
	return err
}

// ErrNotFound 表示资源不存在或无权访问（两者不区分，避免暴露数据是否存在）。
var ErrNotFound = errors.New("lingo: not found")

// fetchAttachment 把 Config.FetchAttachment 适配给 worker；未配置时为 nil。
func (l *Lingo) fetchAttachment() func(context.Context, ticket.Attachment) (io.ReadCloser, error) {
	f := l.cfg.FetchAttachment
	if f == nil {
		return nil
	}
	return func(ctx context.Context, a ticket.Attachment) (io.ReadCloser, error) {
		rc, err := f(ctx, a.UserID, publicAttachment(a))
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
			return nil, fmt.Errorf("%w: %v", ticket.ErrNotFound, err) // 不再重试
		}
		return rc, err
	}
}

// giteaOptions 把公开的 GiteaConfig 转为内部客户端配置；所有 Gitea 请求都经由这一个客户端发出。
func giteaOptions(g GiteaConfig) gitea.Options {
	return gitea.Options{BaseURL: g.BaseURL, Token: g.Token, RejectRedirects: g.RejectRedirects}
}
