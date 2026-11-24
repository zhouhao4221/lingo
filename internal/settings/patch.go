package settings

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// GiteaPatch 是 Gitea 配置的修改：nil 表示不改，指向空串表示清除。
type GiteaPatch struct {
	BaseURL             *string
	Token               *string
	WebhookSecret       *string
	AutoRegisterWebhook *bool
	WebhookURL          *string
	RejectRedirects     *bool
}

// SlotPatch 是模型槽位的修改：nil 表示不改，指向空串表示清除。
type SlotPatch struct {
	Provider *string
	BaseURL  *string
	APIKey   *string
	Model    *string
}

// Patch 是一次修改。
type Patch struct {
	Gitea *GiteaPatch
	Main  *SlotPatch
	Light *SlotPatch
}

// Changed 是一次修改涉及的类别与字段名（不含值，供审计日志）。
type Changed map[Category][]string

// DefaultBaseURL 返回模型类型的默认接口地址（槽位未填地址时使用）。
func DefaultBaseURL(provider string) string {
	switch provider {
	case ProviderDeepSeek:
		return "https://api.deepseek.com"
	case ProviderOpenAI:
		return "https://api.openai.com/v1"
	}
	return ""
}

// Apply 把修改应用到数据库中保存的值，返回新值与涉及的字段；只有含字段的类别需要回写。修改代码配置（Overrides）中
// 已有的项返回 ErrConflict；格式不合法、改了地址却没同时提交对应密钥（避免把已存的密钥发往新地址）返回 ErrInvalid。
func Apply(ov Overrides, st Stored, p Patch) (Stored, Changed, error) {
	out, changed := st, Changed{}
	if gp := p.Gitea; gp != nil {
		g, fields, err := applyGitea(ov.Gitea, st.Gitea, *gp)
		if err != nil {
			return Stored{}, nil, err
		}
		out.Gitea = g
		if len(fields) > 0 {
			changed[CategoryGitea] = fields
		}
	}
	var modelFields []string
	for _, s := range []struct {
		name string
		cfg  bool
		cur  *Slot
		p    *SlotPatch
	}{{"main", ov.Main, &out.Model.Main, p.Main}, {"light", ov.Light, &out.Model.Light, p.Light}} {
		if s.p == nil {
			continue
		}
		if s.cfg && !s.p.empty() {
			return Stored{}, nil, fmt.Errorf("%w: %s model is configured in code", ErrConflict, s.name)
		}
		slot, fields, err := applySlot(s.name, *s.cur, *s.p)
		if err != nil {
			return Stored{}, nil, err
		}
		*s.cur = slot
		for _, f := range fields {
			modelFields = append(modelFields, s.name+"."+f)
		}
	}
	if len(modelFields) > 0 {
		changed[CategoryModel] = modelFields
	}
	// 修改了 Gitea 时，生效后的自动注册须有回调地址与密钥（只改模型时不因 Gitea 配置被拒）
	eff := Resolve(ov, out)
	if p.Gitea != nil && eff.Gitea.AutoRegisterWebhook && (eff.Gitea.WebhookURL == "" || eff.Gitea.WebhookSecret == "") {
		return Stored{}, nil, fmt.Errorf("%w: webhook_url and webhook_secret are required when auto_register_webhook is set", ErrInvalid)
	}
	return out, changed, nil
}

func (p SlotPatch) empty() bool {
	return p.Provider == nil && p.BaseURL == nil && p.APIKey == nil && p.Model == nil
}

func applyGitea(cfg, cur Gitea, p GiteaPatch) (Gitea, []string, error) {
	var fields []string
	str := func(name, cfgVal string, dst *string, v *string, isURL bool) error {
		if v == nil {
			return nil
		}
		if cfgVal != "" {
			return fmt.Errorf("%w: gitea %s is configured in code", ErrConflict, name)
		}
		s := strings.TrimSpace(*v)
		if isURL && s != "" {
			if err := checkURL(s); err != nil {
				return fmt.Errorf("%w: gitea %s %v", ErrInvalid, name, err)
			}
		}
		*dst, fields = s, append(fields, name)
		return nil
	}
	boolean := func(name string, cfgVal bool, dst *bool, v *bool) error {
		if v == nil {
			return nil
		}
		if cfgVal {
			return fmt.Errorf("%w: gitea %s is configured in code", ErrConflict, name)
		}
		*dst, fields = *v, append(fields, name)
		return nil
	}
	out := cur
	for _, err := range []error{
		str(FieldBaseURL, cfg.BaseURL, &out.BaseURL, p.BaseURL, true),
		str(FieldToken, cfg.Token, &out.Token, p.Token, false),
		str(FieldWebhookSecret, cfg.WebhookSecret, &out.WebhookSecret, p.WebhookSecret, false),
		boolean(FieldAutoRegisterWebhook, cfg.AutoRegisterWebhook, &out.AutoRegisterWebhook, p.AutoRegisterWebhook),
		str(FieldWebhookURL, cfg.WebhookURL, &out.WebhookURL, p.WebhookURL, true),
		boolean(FieldRejectRedirects, cfg.RejectRedirects, &out.RejectRedirects, p.RejectRedirects),
	} {
		if err != nil {
			return Gitea{}, nil, err
		}
	}
	// 地址变了（协议或主机）却沿用已有的令牌：令牌会发往新地址。
	//   - 首次设置地址（原来为空）：放行，令牌是为这次接入准备的
	//   - 清空地址：一并清空数据库中的令牌（防止先清空、再改成别处的两步绕过）；令牌由代码配置时无法清空，拒绝
	//   - 改成别的地址：要求同时重新提交令牌；令牌由代码配置时只能改代码
	oldBase, newBase := first(cfg.BaseURL, cur.BaseURL), first(cfg.BaseURL, out.BaseURL)
	if p.Token == nil && first(cfg.Token, cur.Token) != "" && originOf(oldBase) != originOf(newBase) {
		switch {
		case newBase == "" && cfg.Token != "":
			return Gitea{}, nil, fmt.Errorf("%w: the gitea token is configured in code; clear the address in code", ErrConflict)
		case newBase == "":
			if out.Token != "" {
				out.Token, fields = "", append(fields, FieldToken)
			}
		case oldBase == "":
		case cfg.Token != "":
			return Gitea{}, nil, fmt.Errorf("%w: the gitea token is configured in code; change the address in code", ErrConflict)
		default:
			return Gitea{}, nil, fmt.Errorf("%w: changing the gitea address requires re-entering the token", ErrInvalid)
		}
	}
	return out, fields, nil
}

func applySlot(name string, cur Slot, p SlotPatch) (Slot, []string, error) {
	var fields []string
	out := cur
	set := func(f string, dst *string, v *string) {
		if v != nil {
			*dst, fields = strings.TrimSpace(*v), append(fields, f)
		}
	}
	set("provider", &out.Provider, p.Provider)
	set("base_url", &out.BaseURL, p.BaseURL)
	set("api_key", &out.APIKey, p.APIKey)
	set("model", &out.Model, p.Model)
	switch out.Provider {
	case "", ProviderOpenAI:
	case ProviderDeepSeek:
		if out.Model == "" {
			out.Model = DefaultDeepSeekModel
		}
	default:
		return Slot{}, nil, fmt.Errorf("%w: %s model provider must be openai or deepseek", ErrInvalid, name)
	}
	if out.BaseURL != "" {
		if err := checkURL(out.BaseURL); err != nil {
			return Slot{}, nil, fmt.Errorf("%w: %s model base_url %v", ErrInvalid, name, err)
		}
	}
	if (out.Provider == "") != (out.Model == "") {
		return Slot{}, nil, fmt.Errorf("%w: %s model needs both provider and model", ErrInvalid, name)
	}
	// 接口地址变了（协议、主机，含换类型）却沿用已存的 API Key：清空槽位时一并清空 API Key，改成新地址时要求重新提交
	if p.APIKey == nil && cur.APIKey != "" && originOf(endpoint(cur)) != originOf(endpoint(out)) {
		if endpoint(out) != "" {
			return Slot{}, nil, fmt.Errorf("%w: changing the %s model address requires re-entering the api_key", ErrInvalid, name)
		}
		out.APIKey, fields = "", append(fields, "api_key")
	}
	return out, fields, nil
}

// endpoint 是槽位实际请求的地址。
func endpoint(s Slot) string { return first(s.BaseURL, DefaultBaseURL(s.Provider)) }

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// originOf 返回地址的协议与主机（含端口），用于判断密钥会不会发往别处；空串或无法解析为空。
func originOf(s string) string {
	u, err := url.Parse(s)
	if err != nil || s == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// checkURL 只接受 http(s) 绝对地址，不含用户信息、查询与片段（错误信息不回显地址）。
func checkURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil, u.Scheme != "http" && u.Scheme != "https", u.Host == "":
		return fmt.Errorf("must be an absolute http(s) address")
	case u.User != nil, u.RawQuery != "", u.Fragment != "":
		return fmt.Errorf("must not contain user info, query or fragment")
	}
	return nil
}

// Encode 返回需要回写的类别与内容。
func Encode(st Stored, changed Changed) (map[Category]json.RawMessage, error) {
	out := map[Category]json.RawMessage{}
	for c := range changed {
		var v any = st.Gitea
		if c == CategoryModel {
			v = st.Model
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out[c] = b
	}
	return out, nil
}
