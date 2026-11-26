package lingo

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhouhao4221/lingo/internal/assistant"
	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/llm"
	"github.com/zhouhao4221/lingo/internal/settings"
	"github.com/zhouhao4221/lingo/internal/webhook"
	"github.com/zhouhao4221/lingo/internal/worker"
)

// connRefreshEvery 是各实例检查连接配置版本的间隔：保存后其他实例最多晚这么久生效。
var connRefreshEvery = 30 * time.Second

// connState 是当前生效的连接（原子替换）：进行中的请求与任务使用开始时取到的那一份。
type connState struct {
	eff      settings.Effective
	giteaKey string       // 地址 | 令牌 | 是否拒绝重定向：变化时重建 Gitea 客户端配置
	secret   string       // Webhook 密钥
	webhook  http.Handler // 为 nil 时 Webhook 返回 503（未配置密钥）
	main     Model        // 主模型；为 nil 时问答不可用
	light    Model        // 轻量模型（未单独配置时同 main）
	mainSlot settings.Slot
	litSlot  settings.Slot
}

// overrides 是代码配置（lingo.Config）中给出的连接配置，优先于数据库。
func (l *Lingo) overrides() settings.Overrides {
	g := l.cfg.Gitea
	return settings.Overrides{
		Gitea: settings.Gitea{BaseURL: g.BaseURL, Token: g.Token, WebhookSecret: g.WebhookSecret,
			AutoRegisterWebhook: g.AutoRegisterWebhook, WebhookURL: g.WebhookURL, RejectRedirects: g.RejectRedirects},
		Main: !isNil(l.cfg.Model), Light: !isNil(l.cfg.LightModel),
	}
}

// conn 返回当前生效的连接；距上次检查满 connRefreshEvery 时在后台刷新一次（只调用 Go 方法、不挂载 Handler 与 Run
// 的进程也能跟上其他实例保存的配置）。
func (l *Lingo) conn() *connState {
	if l.settings != nil {
		l.touchSettings()
	}
	return l.conns.Load()
}

// applySettings 按数据库中保存的配置重建生效连接。force 为假时只有版本更高才重建（防止并发的旧读覆盖新值）。
// 返回 Gitea 连接是否变化。
func (l *Lingo) applySettings(st settings.Stored, force bool) bool {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	cur := l.conns.Load()
	if cur != nil && !force && !newer(st.GiteaMeta, cur.eff.GiteaMeta) && !newer(st.ModelMeta, cur.eff.ModelMeta) {
		return false
	}
	eff := settings.Resolve(l.overrides(), st)
	next := &connState{eff: eff}
	if cur != nil {
		*next = *cur
		next.eff = eff
	}
	// Gitea 客户端：同一个实例原子替换配置，持有它的各组件不用重建
	g := eff.Gitea
	key := ""
	if eff.GiteaConfigured() {
		key = g.BaseURL + "|" + g.Token + "|" + boolStr(g.RejectRedirects)
	}
	giteaChanged := cur == nil || key != cur.giteaKey
	if giteaChanged {
		if key == "" {
			l.gitea.Unconfigure()
		} else if err := l.gitea.Reconfigure(gitea.Options{BaseURL: g.BaseURL, Token: g.Token, RejectRedirects: g.RejectRedirects}); err != nil {
			// 新配置不可用：整个 Gitea 部分（地址、密钥、来源、版本）都保留原值，保持一致；版本不前进，下次刷新会重试
			l.log.Error("lingo: invalid gitea settings, keeping the previous connection", "err", err)
			key = ""
			if cur != nil {
				key = cur.giteaKey
				eff.Gitea, eff.GiteaSrc, eff.GiteaMeta = cur.eff.Gitea, cur.eff.GiteaSrc, cur.eff.GiteaMeta
			} else {
				eff.Gitea, eff.GiteaSrc = settings.Gitea{}, map[string]settings.Source{}
			}
			next.eff, g = eff, eff.Gitea
			giteaChanged = false
		}
		next.giteaKey = key
		if giteaChanged {
			l.resetBot()
			l.tmpls.Reset()
			if l.worker != nil {
				l.worker.ResetLabels()
			}
			if cur != nil && (cur.giteaKey == "") != (key == "") {
				l.log.Info("lingo: gitea connection state changed", "configured", key != "")
			}
		}
	}
	// Webhook 验签：密钥变化时换处理器（按新密钥验签）
	if cur == nil || g.WebhookSecret != cur.secret {
		next.secret, next.webhook = g.WebhookSecret, nil
		if g.WebhookSecret != "" {
			h, err := webhook.NewHandler(webhook.Options{Secret: g.WebhookSecret, BotLogin: l.botLogin, ReplyMode: l.replyMode(),
				Store: l.store, Logger: l.log})
			if err != nil {
				l.log.Error("lingo: webhook handler", "err", err)
			} else {
				next.webhook = h
			}
		}
	}
	// 模型：代码配置的槽位直接用；数据库配置的槽位变化时重建
	next.main, next.mainSlot = l.slotModel(l.cfg.Model, eff.MainSrc, eff.Main, cur, true)
	next.light, next.litSlot = l.slotModel(l.cfg.LightModel, eff.LightSrc, eff.Light, cur, false)
	if isNil(next.light) {
		next.light = next.main
	}
	l.conns.Store(next)
	if giteaChanged && key != "" {
		go l.botLogin() // 立即获取新连接的机器人账号，缩短依赖它的任务被推迟的窗口
	}
	// Gitea 的任何连接项（含 Webhook 密钥、回调地址、自动注册）变化后，由 Run 补跑一次自检：开启自动注册时把新的
	// 密钥与地址同步到各仓库的 Webhook
	if cur != nil && key != "" && eff.Gitea != cur.eff.Gitea {
		l.selfCheckDue.Store(true)
	}
	if cur != nil && cur.eff.Gitea.WebhookURL != "" && cur.eff.Gitea.WebhookURL != eff.Gitea.WebhookURL {
		l.prevWebhooks.add(cur.eff.Gitea.WebhookURL) // 自检时把指向旧地址的 Hook 改到新地址
	}
	return giteaChanged
}

// newer 报告 m 是否比 cur 新：按版本号判断。唯一的例外是配置行被补建：版本从 1 重新开始，此时版本为 1 且修改时间
// 更晚才算更新。修改时间是事务开始的时间、不随提交顺序，所以其他情况都不用它（并发保存时较旧的结果不会覆盖较新的）。
func newer(m, cur settings.Meta) bool {
	switch {
	case m.Version > cur.Version:
		return true
	case m.Version == 1 && cur.Version > 1:
		return m.UpdatedAt != nil && (cur.UpdatedAt == nil || m.UpdatedAt.After(*cur.UpdatedAt))
	}
	return false
}

func (l *Lingo) slotModel(cfg Model, src settings.Source, slot settings.Slot, cur *connState, main bool) (Model, settings.Slot) {
	switch src {
	case settings.SourceConfig:
		return cfg, settings.Slot{}
	case settings.SourceDatabase:
		if cur != nil {
			prev, prevSlot := cur.main, cur.mainSlot
			if !main {
				prev, prevSlot = cur.light, cur.litSlot
			}
			if prevSlot == slot && !isNil(prev) {
				return prev, slot
			}
		}
		m, err := buildModel(slot)
		if err != nil {
			l.log.Error("lingo: invalid model settings", "slot", map[bool]string{true: "main", false: "light"}[main], "err", err)
			return nil, settings.Slot{}
		}
		return m, slot
	}
	return nil, settings.Slot{}
}

// buildModel 用数据库中的槽位创建内置模型（OpenAI 兼容接口）。
func buildModel(s settings.Slot) (Model, error) {
	base := s.BaseURL
	if base == "" {
		base = settings.DefaultBaseURL(s.Provider)
	}
	return NewOpenAIModel(OpenAIConfig{BaseURL: base, APIKey: s.APIKey, Model: s.Model, Images: s.Images})
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// refreshSettings 读取数据库中的配置，版本更高时应用；读取失败（数据库不可用、未执行迁移 0004）保留当前连接。
func (l *Lingo) refreshSettings(ctx context.Context) {
	st, err := l.settings.Load(ctx)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("lingo: load connection settings failed, keeping the current connection (run lingo.Migrate?)", "err", err)
		}
		return
	}
	l.applyLoaded(st)
}

// applyLoaded 应用刚从数据库读到的配置：已提交值就是最新，与缓存不一致（版本或修改时间不同）就应用。配置行从备份
// 恢复到较低版本时也能跟上；读到的若恰好早于本实例刚提交的保存，下一次刷新即纠正。
func (l *Lingo) applyLoaded(st settings.Stored) {
	if c := l.conns.Load(); c == nil || !sameMeta(st.GiteaMeta, c.eff.GiteaMeta) || !sameMeta(st.ModelMeta, c.eff.ModelMeta) {
		l.applySettings(st, true)
	}
}

func sameMeta(a, b settings.Meta) bool {
	return a.Version == b.Version && (a.UpdatedAt == nil) == (b.UpdatedAt == nil) && (a.UpdatedAt == nil || a.UpdatedAt.Equal(*b.UpdatedAt))
}

// touchSettings 在距上次检查满 connRefreshEvery 时于后台刷新一次（不阻塞请求）；Handler 与 Run 分进程部署时
// 只运行 Handler 的进程靠它跟上其他实例保存的配置。
func (l *Lingo) touchSettings() {
	now := time.Now().UnixNano()
	if l.settingsStopped.Load() || now-l.settingsChecked.Load() < int64(connRefreshEvery) || !l.settingsRefreshing.CompareAndSwap(false, true) {
		return
	}
	l.settingsChecked.Store(now)
	l.settingsWG.Add(1)
	go func() {
		defer l.settingsWG.Done()
		defer l.settingsRefreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l.refreshSettings(ctx)
	}()
}

// watchSettings 在 Run 中每 connRefreshEvery 检查一次配置版本；Gitea 连接变化后补跑一次自检。
func (l *Lingo) watchSettings(ctx context.Context, selfCheck func()) {
	t := time.NewTicker(connRefreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.settingsChecked.Store(time.Now().UnixNano())
			l.refreshSettings(ctx)
		case <-l.settingsChanged:
		}
		if l.selfCheckDue.CompareAndSwap(true, false) && !l.cfg.SkipSelfCheck {
			selfCheck()
		}
	}
}

// liveHandler 在处理请求前按需刷新连接配置。
func (l *Lingo) liveHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.touchSettings()
		h.ServeHTTP(w, r)
	})
}

// mainModel 返回当前的主模型；未配置时为 nil。
func (l *Lingo) mainModel() Model {
	if c := l.conn(); c != nil {
		return c.main
	}
	return nil
}

// drafterFor 返回当前的建单整理模型（LightModel，未配置时为 Model）；都没有时为 nil（按原内容建单）。
func (l *Lingo) drafterFor() worker.Drafter {
	c := l.conn()
	if c == nil || isNil(c.light) {
		return nil
	}
	return &llm.Drafter{Chat: chatOf(c.light)}
}

// brainFor 返回当前的问答模型调用；未配置主模型时为 nil（问答返回 ErrNoModel）。
func (l *Lingo) brainFor() assistant.Brain {
	c := l.conn()
	if c == nil || isNil(c.main) {
		return nil
	}
	light := c.light
	if isNil(light) {
		light = c.main
	}
	return &llm.Assistant{Light: chatOf(light), Main: chatOf(c.main), MainStream: streamOf(c.main), Record: l.recordUsage,
		MainImages: supportsImages(c.main), LightImages: supportsImages(light)}
}

// settingsState 是 Lingo 中与连接配置相关的字段（嵌入 Lingo）。
type settingsState struct {
	settings           *settings.Service
	conns              atomic.Pointer[connState]
	settingsChecked    atomic.Int64 // 上次检查的时间（UnixNano）
	settingsRefreshing atomic.Bool
	settingsChanged    chan struct{} // 保存后通知 Run 的 watchSettings
	selfCheckDue       atomic.Bool
	settingsStopped    atomic.Bool    // Run 已退出：不再发起后台刷新
	settingsWG         sync.WaitGroup // 进行中的后台刷新（Run 退出前等待）
	prevWebhooks       webhookHistory
}

// webhookHistory 记录运行时改过的旧回调地址，供自检把指向旧地址的 Hook 改到新地址。
type webhookHistory struct {
	mu   sync.Mutex
	urls []string
}

func (h *webhookHistory) add(u string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Contains(h.urls, u) {
		h.urls = append(h.urls, u)
	}
}

func (h *webhookHistory) list() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.urls)
}
