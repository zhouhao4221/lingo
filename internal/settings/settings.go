package settings

import (
	"encoding/json"
	"errors"
	"time"
)

// Category 是配置类别，数据库中每类一行。
type Category string

const (
	CategoryModel Category = "model"
	CategoryGitea Category = "gitea"
)

// Source 是一项配置的来源。
type Source string

const (
	SourceConfig   Source = "config"   // 代码中的 lingo.Config，优先，不能经接口修改
	SourceDatabase Source = "database" // 数据库中保存的值
	SourceUnset    Source = "unset"
)

// 模型类型。
const (
	ProviderOpenAI   = "openai"   // OpenAI 及兼容接口
	ProviderDeepSeek = "deepseek" // DeepSeek
)

// DefaultDeepSeekModel 是 DeepSeek 未填模型名时使用的模型。
const DefaultDeepSeekModel = "deepseek-chat"

var (
	// ErrInvalid 表示配置不合法。
	ErrInvalid = errors.New("settings: invalid input")
	// ErrConflict 表示要修改的项由代码配置（lingo.Config），不能经接口修改。
	ErrConflict = errors.New("settings: conflict")
)

// Gitea 是 Gitea 连接配置。
type Gitea struct {
	BaseURL             string `json:"base_url,omitempty"`
	Token               string `json:"token,omitempty"`
	WebhookSecret       string `json:"webhook_secret,omitempty"`
	AutoRegisterWebhook bool   `json:"auto_register_webhook,omitempty"`
	WebhookURL          string `json:"webhook_url,omitempty"`
	RejectRedirects     bool   `json:"reject_redirects,omitempty"`
}

// Slot 是一个模型槽位（主模型或轻量模型）。
type Slot struct {
	Provider string `json:"provider,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Configured 报告槽位是否已配置（类型与模型名都有值；API Key 可空，兼容本地服务）。
func (s Slot) Configured() bool { return s.Provider != "" && s.Model != "" }

// Model 是模型配置。
type Model struct {
	Main  Slot `json:"main"`
	Light Slot `json:"light"`
}

// Meta 是一类配置的版本与最近修改。
type Meta struct {
	Version   int64
	UpdatedBy string
	UpdatedAt *time.Time
}

// Record 是数据库中的一行。
type Record struct {
	Category  Category
	Content   json.RawMessage
	Version   int64
	UpdatedBy string
	UpdatedAt *time.Time
}

// Stored 是数据库中保存的全部连接配置。
type Stored struct {
	Gitea     Gitea
	Model     Model
	GiteaMeta Meta
	ModelMeta Meta
}

// Decode 把数据库行解码为 Stored（未知字段忽略，缺少的类别为零值）。
func Decode(rs []Record) (Stored, error) {
	var st Stored
	for _, r := range rs {
		meta := Meta{Version: r.Version, UpdatedBy: r.UpdatedBy, UpdatedAt: r.UpdatedAt}
		var err error
		switch r.Category {
		case CategoryGitea:
			err, st.GiteaMeta = decode(r.Content, &st.Gitea), meta
		case CategoryModel:
			err, st.ModelMeta = decode(r.Content, &st.Model), meta
		}
		if err != nil {
			return Stored{}, err
		}
	}
	return st, nil
}

func decode(b json.RawMessage, v any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// Overrides 是代码中的 lingo.Config 给出的值：Gitea 中非空的字符串、为 true 的布尔项优先；
// Main / Light 表示 Config.Model / LightModel 已设置（模型是接口对象，按槽位整体优先）。
type Overrides struct {
	Gitea Gitea
	Main  bool
	Light bool
}

// GiteaField 是 Gitea 配置项的名称（与 JSON 字段一致）。
const (
	FieldBaseURL             = "base_url"
	FieldToken               = "token"
	FieldWebhookSecret       = "webhook_secret"
	FieldAutoRegisterWebhook = "auto_register_webhook"
	FieldWebhookURL          = "webhook_url"
	FieldRejectRedirects     = "reject_redirects"
)

// Effective 是生效的连接配置。
type Effective struct {
	Gitea     Gitea
	GiteaSrc  map[string]Source // 按 Field* 名称
	Main      Slot              // MainSrc 为 database 时有效
	Light     Slot              // LightSrc 为 database 时有效
	MainSrc   Source
	LightSrc  Source
	GiteaMeta Meta
	ModelMeta Meta
}

// Resolve 合并代码配置与数据库配置：字符串项代码中非空则为 config，否则数据库非空为 database，否则 unset；
// 布尔项代码中为 true 则为 config，否则取数据库（保存过即为 database）；模型按槽位，代码中设置了则整体为 config。
func Resolve(ov Overrides, st Stored) Effective {
	e := Effective{GiteaSrc: map[string]Source{}, GiteaMeta: st.GiteaMeta, ModelMeta: st.ModelMeta}
	str := func(name, cfg, db string) string {
		switch {
		case cfg != "":
			e.GiteaSrc[name] = SourceConfig
			return cfg
		case db != "":
			e.GiteaSrc[name] = SourceDatabase
			return db
		}
		e.GiteaSrc[name] = SourceUnset
		return ""
	}
	boolean := func(name string, cfg, db bool) bool {
		switch {
		case cfg:
			e.GiteaSrc[name] = SourceConfig
			return true
		case st.GiteaMeta.Version > 0 || db:
			e.GiteaSrc[name] = SourceDatabase
			return db
		}
		e.GiteaSrc[name] = SourceUnset
		return false
	}
	g, c := st.Gitea, ov.Gitea
	e.Gitea = Gitea{
		BaseURL:             str(FieldBaseURL, c.BaseURL, g.BaseURL),
		Token:               str(FieldToken, c.Token, g.Token),
		WebhookSecret:       str(FieldWebhookSecret, c.WebhookSecret, g.WebhookSecret),
		AutoRegisterWebhook: boolean(FieldAutoRegisterWebhook, c.AutoRegisterWebhook, g.AutoRegisterWebhook),
		WebhookURL:          str(FieldWebhookURL, c.WebhookURL, g.WebhookURL),
		RejectRedirects:     boolean(FieldRejectRedirects, c.RejectRedirects, g.RejectRedirects),
	}
	slot := func(cfg bool, db Slot) (Slot, Source) {
		switch {
		case cfg:
			return Slot{}, SourceConfig
		case db.Configured():
			return db, SourceDatabase
		}
		return Slot{}, SourceUnset
	}
	e.Main, e.MainSrc = slot(ov.Main, st.Model.Main)
	e.Light, e.LightSrc = slot(ov.Light, st.Model.Light)
	return e
}

// GiteaConfigured 报告 Gitea 是否已配置（地址与令牌都有值）。
func (e Effective) GiteaConfigured() bool { return e.Gitea.BaseURL != "" && e.Gitea.Token != "" }

// ModelConfigured 报告主模型是否已配置。
func (e Effective) ModelConfigured() bool { return e.MainSrc != SourceUnset }

// Mask 返回密钥的掩码：空串为空；不足 12 个字符为 "****"；否则为前 3 位 + "****" + 后 4 位（如 "sk-****abcd"）。
func Mask(s string) string {
	switch r := []rune(s); {
	case len(r) == 0:
		return ""
	case len(r) < 12:
		return "****"
	default:
		return string(r[:3]) + "****" + string(r[len(r)-4:])
	}
}
