package lingo

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/llm"
	"github.com/zhouhao4221/lingo/internal/settings"
)

// SettingSource 是一项连接配置的来源：config（代码中的 lingo.Config，优先，不能经接口修改）、
// database（数据库中保存的值）、unset（未配置）。
type SettingSource string

const (
	SourceConfig   SettingSource = "config"
	SourceDatabase SettingSource = "database"
	SourceUnset    SettingSource = "unset"
)

// StringSetting 是一项字符串配置。
type StringSetting struct {
	Value  string        `json:"value"`
	Source SettingSource `json:"source"`
}

// BoolSetting 是一项开关配置。
type BoolSetting struct {
	Value  bool          `json:"value"`
	Source SettingSource `json:"source"`
}

// SecretSetting 是一项密钥配置：只返回是否已设置与掩码（如 "sk-****abcd"），任何接口都不返回原文。
type SecretSetting struct {
	Set    bool          `json:"set"`
	Masked string        `json:"masked,omitempty"`
	Source SettingSource `json:"source"`
}

// ModelSlotSettings 是一个模型槽位（主模型或轻量模型）。来源为 config 时，模型由代码中的 Config.Model / LightModel
// 提供（接口对象，读不到其中的配置），其余字段为空，APIKey.Set 也不表示是否配置了 key。
type ModelSlotSettings struct {
	Source   SettingSource `json:"source"`
	Provider string        `json:"provider,omitempty"` // openai | deepseek
	BaseURL  string        `json:"base_url,omitempty"` // 为空时用类型的默认地址
	Model    string        `json:"model,omitempty"`
	APIKey   SecretSetting `json:"api_key"`
}

// SettingsMeta 是一类配置的版本（每次保存 +1）与最近修改。
type SettingsMeta struct {
	Version   int64      `json:"version"`
	UpdatedBy string     `json:"updated_by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// ModelConnection 是模型连接配置。Configured 表示主模型已配置（问答可用）；轻量模型未配置时用主模型。
type ModelConnection struct {
	Configured bool `json:"configured"`
	// Applied 表示本实例正在使用这份配置；为假时配置保存了但没有生效（如模型创建失败），详见日志
	Applied bool              `json:"applied"`
	Main    ModelSlotSettings `json:"main"`
	Light   ModelSlotSettings `json:"light"`
	SettingsMeta
}

// GiteaConnection 是 Gitea 连接配置。Configured 表示地址与令牌都有值。
type GiteaConnection struct {
	Configured bool `json:"configured"`
	// Applied 表示本实例正在使用这份配置；为假时配置保存了但没有生效（如地址不可用，仍用原连接），详见日志
	Applied             bool          `json:"applied"`
	BaseURL             StringSetting `json:"base_url"`
	Token               SecretSetting `json:"token"`
	WebhookSecret       SecretSetting `json:"webhook_secret"`
	AutoRegisterWebhook BoolSetting   `json:"auto_register_webhook"`
	WebhookURL          StringSetting `json:"webhook_url"`
	RejectRedirects     BoolSetting   `json:"reject_redirects"`
	SettingsMeta
}

// ConnectionSettings 是当前生效的连接配置（代码配置与数据库合并后）。
type ConnectionSettings struct {
	Configured bool            `json:"configured"` // 模型与 Gitea 都已配置
	Model      ModelConnection `json:"model"`
	Gitea      GiteaConnection `json:"gitea"`
}

// ModelSlotUpdate 是模型槽位的修改：字段为 nil 表示不改，指向空串表示清除。
type ModelSlotUpdate struct {
	Provider *string `json:"provider"`
	BaseURL  *string `json:"base_url"`
	APIKey   *string `json:"api_key"`
	Model    *string `json:"model"`
}

// ModelConnectionUpdate 是模型连接配置的修改。
type ModelConnectionUpdate struct {
	Main  *ModelSlotUpdate `json:"main"`
	Light *ModelSlotUpdate `json:"light"`
}

// GiteaConnectionUpdate 是 Gitea 连接配置的修改：字段为 nil 表示不改，指向空串表示清除。
// 修改地址（主机变化）时必须同时提交令牌，避免把已保存的令牌发往新地址；模型的地址与 API Key 同理。
type GiteaConnectionUpdate struct {
	BaseURL             *string `json:"base_url"`
	Token               *string `json:"token"`
	WebhookSecret       *string `json:"webhook_secret"`
	AutoRegisterWebhook *bool   `json:"auto_register_webhook"`
	WebhookURL          *string `json:"webhook_url"`
	RejectRedirects     *bool   `json:"reject_redirects"`
}

// ConnectionSettingsUpdate 是一次修改；Model / Gitea 为 nil 表示该类不改。
type ConnectionSettingsUpdate struct {
	Model *ModelConnectionUpdate `json:"model"`
	Gitea *GiteaConnectionUpdate `json:"gitea"`
}

// ConnectionCheck 是一项连通测试的结果。Message 已脱敏：不含地址、密钥与上游响应原文。
type ConnectionCheck struct {
	OK bool `json:"ok"`
	// Code 取 ok | not_configured | auth_failed | forbidden | not_found | redirect | timeout | unreachable | tls |
	// http_error | invalid_response | error
	Code    string `json:"code"`
	Status  int    `json:"status,omitempty"` // HTTP 状态码（有时）
	Message string `json:"message"`
	Login   string `json:"login,omitempty"`   // Gitea：机器人账号
	Version string `json:"version,omitempty"` // Gitea：版本
}

// ConnectionTestResult 是连通测试的结果。LightModel 只在轻量模型单独配置时返回。
type ConnectionTestResult struct {
	Model      ConnectionCheck  `json:"model"`
	LightModel *ConnectionCheck `json:"light_model,omitempty"`
	Gitea      ConnectionCheck  `json:"gitea"`
}

// authorizeSettings 判断 u 能否查看（write 为假）或修改、测试（write 为真）连接配置：Config.AuthorizeSettings 优先，
// 未配置时只允许 admin。
func (l *Lingo) authorizeSettings(ctx context.Context, u User, write bool) error {
	if f := l.cfg.AuthorizeSettings; f != nil {
		err := f(ctx, u, write)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrNotFound):
			return ErrNotFound
		case errors.Is(err, ErrForbidden):
			return ErrForbidden
		}
		return err
	}
	if u.Role != RoleAdmin {
		return ErrForbidden
	}
	return nil
}

// ConnectionSettings 返回当前生效的连接配置（模型与 Gitea），密钥只返回掩码。权限见 Config.AuthorizeSettings。
func (l *Lingo) ConnectionSettings(ctx context.Context, u User) (ConnectionSettings, error) {
	if err := l.authorizeSettings(ctx, u, false); err != nil {
		return ConnectionSettings{}, err
	}
	st, err := l.settings.Load(ctx)
	if err != nil {
		return ConnectionSettings{}, err
	}
	l.applyLoaded(st) // 顺带跟上其他实例保存的配置（读到的是数据库中的已提交值）
	return l.markApplied(publicSettings(settings.Resolve(l.overrides(), st), st), st), nil
}

// UpdateConnectionSettings 按字段修改并保存连接配置（明文存入数据库），当前实例立即生效，其他实例 30 秒内生效；
// 进行中的请求与任务使用开始时的配置。修改代码中已配置的项返回 ErrConflict；格式不合法、改了地址却没同时提交对应
// 密钥返回 ErrInvalidInput。每次保存写一条应用日志（修改人、类别、字段名，不含值）。
func (l *Lingo) UpdateConnectionSettings(ctx context.Context, u User, in ConnectionSettingsUpdate) (ConnectionSettings, error) {
	if err := l.authorizeSettings(ctx, u, true); err != nil {
		return ConnectionSettings{}, err
	}
	st, changed, err := l.settings.Update(ctx, u.ID, settingsPatch(in))
	if err != nil {
		return ConnectionSettings{}, settingsErr(err)
	}
	for cat, fields := range changed {
		meta := st.GiteaMeta
		if cat == settings.CategoryModel {
			meta = st.ModelMeta
		}
		l.log.Info("lingo: connection settings updated", "by", u.ID, "category", string(cat), "fields", fields, "version", meta.Version)
	}
	if len(changed) > 0 {
		l.applySettings(st, false) // 按版本或修改时间判断：并发保存时不会让较旧的结果覆盖较新的
		select {                   // 通知 Run：Gitea 连接变化时补跑自检
		case l.settingsChanged <- struct{}{}:
		default:
		}
	}
	return l.markApplied(publicSettings(settings.Resolve(l.overrides(), st), st), st), nil
}

// markApplied 按本实例实际生效的连接标出各类配置是否已生效。
func (l *Lingo) markApplied(s ConnectionSettings, st settings.Stored) ConnectionSettings {
	c := l.conn()
	if c == nil {
		return s
	}
	want := settings.Resolve(l.overrides(), st)
	// 本实例已在用同样的值，或已应用了更新的版本（保存的同时后台刷新到了别人的新保存）都算已生效
	s.Gitea.Applied = c.eff.Gitea == want.Gitea || c.eff.GiteaMeta.Version > st.GiteaMeta.Version
	newerModel := c.eff.ModelMeta.Version > st.ModelMeta.Version
	s.Model.Applied = want.MainSrc == settings.SourceUnset || !isNil(c.main)
	if want.MainSrc == settings.SourceDatabase && c.mainSlot != want.Main {
		s.Model.Applied = false
	}
	if want.LightSrc == settings.SourceDatabase && c.litSlot != want.Light {
		s.Model.Applied = false // 轻量模型没有建起来（退回主模型）
	}
	if newerModel {
		s.Model.Applied = true
	}
	return s
}

// TestConnectionSettings 测试连接（不保存）：in 中的字段覆盖当前值（可为空，测试当前配置）。Gitea 读取机器人账号与
// 版本，模型发一次简短对话；连不通时也返回结果（OK 为假），只有参数错误与权限错误返回 error。会向填写的地址发出请求，
// 只对有修改权限的人开放。
func (l *Lingo) TestConnectionSettings(ctx context.Context, u User, in ConnectionSettingsUpdate) (ConnectionTestResult, error) {
	if err := l.authorizeSettings(ctx, u, true); err != nil {
		return ConnectionTestResult{}, err
	}
	st, err := l.settings.Load(ctx)
	if err != nil {
		return ConnectionTestResult{}, err
	}
	ov := l.overrides()
	next, err := settings.Preview(ov, st, settingsPatch(in))
	if err != nil {
		return ConnectionTestResult{}, settingsErr(err)
	}
	eff := settings.Resolve(ov, next)
	var out ConnectionTestResult
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); out.Gitea = testGitea(ctx, eff.Gitea, eff.GiteaConfigured()) }()
	go func() {
		defer wg.Done()
		out.Model = l.testModel(ctx, l.cfg.Model, eff.MainSrc, eff.Main)
		if eff.LightSrc != settings.SourceUnset {
			c := l.testModel(ctx, l.cfg.LightModel, eff.LightSrc, eff.Light)
			out.LightModel = &c
		}
	}()
	wg.Wait()
	return out, nil
}

func testGitea(ctx context.Context, g settings.Gitea, configured bool) ConnectionCheck {
	if !configured {
		return ConnectionCheck{Code: "not_configured", Message: "Gitea 未配置（地址或令牌为空）"}
	}
	c, err := gitea.New(gitea.Options{BaseURL: g.BaseURL, Token: g.Token, RejectRedirects: g.RejectRedirects,
		HTTPClient: &http.Client{Timeout: 10 * time.Second}})
	if err != nil {
		return ConnectionCheck{Code: "error", Message: "Gitea 地址不合法"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	me, err := c.GetCurrentUser(ctx)
	if err != nil {
		return checkFailure("Gitea", err)
	}
	out := ConnectionCheck{OK: true, Code: "ok", Message: "连接成功", Login: me.Login}
	if v, err := c.GetVersion(ctx); err == nil {
		out.Version = v
	}
	return out
}

func (l *Lingo) testModel(ctx context.Context, cfg Model, src settings.Source, slot settings.Slot) ConnectionCheck {
	var m Model
	switch src {
	case settings.SourceConfig:
		m = cfg
	case settings.SourceDatabase:
		base := slot.BaseURL
		if base == "" {
			base = settings.DefaultBaseURL(slot.Provider)
		}
		cl, err := llm.NewOpenAI(llm.OpenAIOptions{BaseURL: base, APIKey: slot.APIKey, Model: slot.Model, MaxRetries: -1,
			HTTPClient: &http.Client{Timeout: 30 * time.Second}})
		if err != nil {
			return ConnectionCheck{Code: "error", Message: "模型配置不合法"}
		}
		m = openAIModel{cl}
	default:
		return ConnectionCheck{Code: "not_configured", Message: "模型未配置"}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := m.Chat(ctx, []Message{{Role: "user", Content: "ping"}}); err != nil && !errors.Is(err, llm.ErrTruncated) {
		return checkFailure("模型", err)
	}
	return ConnectionCheck{OK: true, Code: "ok", Message: "连接成功"}
}

// checkFailure 按错误类别给出脱敏的失败说明（不输出错误原文、地址或上游响应）。
func checkFailure(what string, err error) ConnectionCheck {
	var ge *gitea.APIError
	var le *llm.APIError
	status := 0
	switch {
	case errors.As(err, &ge):
		status = ge.StatusCode
	case errors.As(err, &le):
		status = le.StatusCode
	}
	var de *gitea.DecodeError
	var ne net.Error
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ce x509.CertificateInvalidError
	switch {
	case status == http.StatusUnauthorized:
		return ConnectionCheck{Code: "auth_failed", Status: status, Message: fmt.Sprintf("%s认证失败（HTTP 401），请检查令牌或 API Key", what)}
	case status == http.StatusForbidden:
		return ConnectionCheck{Code: "forbidden", Status: status, Message: fmt.Sprintf("%s拒绝访问（HTTP 403）", what)}
	case status == http.StatusNotFound:
		return ConnectionCheck{Code: "not_found", Status: status, Message: fmt.Sprintf("%s接口不存在（HTTP 404），请检查地址", what)}
	case status != 0:
		return ConnectionCheck{Code: "http_error", Status: status, Message: fmt.Sprintf("%s返回错误（HTTP %d）", what, status)}
	case errors.Is(err, gitea.ErrRedirect):
		return ConnectionCheck{Code: "redirect", Message: what + "返回了重定向，已按配置拒绝跟随，请核对地址"}
	case errors.As(err, &de), errors.Is(err, llm.ErrEmptyContent):
		return ConnectionCheck{Code: "invalid_response", Message: what + "返回的内容无法识别，请检查地址"}
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return ConnectionCheck{Code: "timeout", Message: what + "连接超时"}
	case errors.As(err, &ua), errors.As(err, &he), errors.As(err, &ce):
		return ConnectionCheck{Code: "tls", Message: what + "的 TLS 证书无法验证"}
	case errors.As(err, &ne):
		return ConnectionCheck{Code: "unreachable", Message: what + "无法连接，请检查地址与网络"}
	}
	return ConnectionCheck{Code: "error", Message: what + "连接失败"}
}

func settingsErr(err error) error {
	switch {
	case errors.Is(err, settings.ErrConflict):
		return fmt.Errorf("%w%s", ErrConflict, trimPrefix(err, settings.ErrConflict))
	case errors.Is(err, settings.ErrInvalid):
		return fmt.Errorf("%w%s", ErrInvalidInput, trimPrefix(err, settings.ErrInvalid))
	}
	return err
}

func trimPrefix(err, sentinel error) string {
	s, p := err.Error(), sentinel.Error()
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return ": " + s
}

func settingsPatch(in ConnectionSettingsUpdate) settings.Patch {
	var p settings.Patch
	if g := in.Gitea; g != nil {
		p.Gitea = &settings.GiteaPatch{BaseURL: g.BaseURL, Token: g.Token, WebhookSecret: g.WebhookSecret,
			AutoRegisterWebhook: g.AutoRegisterWebhook, WebhookURL: g.WebhookURL, RejectRedirects: g.RejectRedirects}
	}
	if m := in.Model; m != nil {
		slot := func(s *ModelSlotUpdate) *settings.SlotPatch {
			if s == nil {
				return nil
			}
			return &settings.SlotPatch{Provider: s.Provider, BaseURL: s.BaseURL, APIKey: s.APIKey, Model: s.Model}
		}
		p.Main, p.Light = slot(m.Main), slot(m.Light)
	}
	return p
}

func publicSettings(e settings.Effective, st settings.Stored) ConnectionSettings {
	src := func(s settings.Source) SettingSource { return SettingSource(s) }
	secret := func(name, v string) SecretSetting {
		return SecretSetting{Set: v != "", Masked: settings.Mask(v), Source: src(e.GiteaSrc[name])}
	}
	meta := func(m settings.Meta) SettingsMeta {
		return SettingsMeta{Version: m.Version, UpdatedBy: m.UpdatedBy, UpdatedAt: m.UpdatedAt}
	}
	slot := func(s settings.Slot, source settings.Source) ModelSlotSettings {
		out := ModelSlotSettings{Source: src(source), APIKey: SecretSetting{Source: src(source)}}
		if source == settings.SourceDatabase {
			out.Provider, out.BaseURL, out.Model = s.Provider, s.BaseURL, s.Model
			out.APIKey.Set, out.APIKey.Masked = s.APIKey != "", settings.Mask(s.APIKey)
		}
		return out
	}
	g := e.Gitea
	out := ConnectionSettings{
		Model: ModelConnection{Configured: e.ModelConfigured(), Main: slot(e.Main, e.MainSrc), Light: slot(e.Light, e.LightSrc),
			SettingsMeta: meta(st.ModelMeta)},
		Gitea: GiteaConnection{Configured: e.GiteaConfigured(),
			BaseURL:             StringSetting{Value: g.BaseURL, Source: src(e.GiteaSrc[settings.FieldBaseURL])},
			Token:               secret(settings.FieldToken, g.Token),
			WebhookSecret:       secret(settings.FieldWebhookSecret, g.WebhookSecret),
			AutoRegisterWebhook: BoolSetting{Value: g.AutoRegisterWebhook, Source: src(e.GiteaSrc[settings.FieldAutoRegisterWebhook])},
			WebhookURL:          StringSetting{Value: g.WebhookURL, Source: src(e.GiteaSrc[settings.FieldWebhookURL])},
			RejectRedirects:     BoolSetting{Value: g.RejectRedirects, Source: src(e.GiteaSrc[settings.FieldRejectRedirects])},
			SettingsMeta:        meta(st.GiteaMeta)},
	}
	out.Configured = out.Model.Configured && out.Gitea.Configured
	return out
}
