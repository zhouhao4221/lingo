package settings

import (
	"errors"
	"slices"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestResolveSources(t *testing.T) {
	st := Stored{Gitea: Gitea{BaseURL: "https://db.example.com", Token: "db-token", RejectRedirects: true},
		Model: Model{Main: Slot{Provider: ProviderDeepSeek, Model: "deepseek-chat", APIKey: "k"}}, GiteaMeta: Meta{Version: 1}}
	e := Resolve(Overrides{Gitea: Gitea{BaseURL: "https://cfg.example.com"}, Light: true}, st)
	switch {
	case e.Gitea.BaseURL != "https://cfg.example.com", e.GiteaSrc[FieldBaseURL] != SourceConfig,
		e.Gitea.Token != "db-token", e.GiteaSrc[FieldToken] != SourceDatabase,
		e.GiteaSrc[FieldWebhookSecret] != SourceUnset,
		!e.Gitea.RejectRedirects, e.GiteaSrc[FieldRejectRedirects] != SourceDatabase,
		e.MainSrc != SourceDatabase, e.LightSrc != SourceConfig, !e.GiteaConfigured(), !e.ModelConfigured():
		t.Errorf("effective = %+v", e)
	}
	// 从未保存过的布尔项为 unset；代码中为 true 时为 config
	e = Resolve(Overrides{Gitea: Gitea{AutoRegisterWebhook: true}}, Stored{})
	if e.GiteaSrc[FieldRejectRedirects] != SourceUnset || e.GiteaSrc[FieldAutoRegisterWebhook] != SourceConfig || e.GiteaConfigured() || e.ModelConfigured() {
		t.Errorf("empty = %+v", e)
	}
}

func TestApply(t *testing.T) {
	base := Stored{Gitea: Gitea{BaseURL: "https://git.example.com", Token: "old-token", WebhookSecret: "s"},
		Model: Model{Main: Slot{Provider: ProviderOpenAI, Model: "gpt", APIKey: "sk-old"}}}
	// 只改令牌：其他字段不变，记录字段名
	st, ch, err := Apply(Overrides{}, base, Patch{Gitea: &GiteaPatch{Token: ptr(" new-token ")}})
	if err != nil || st.Gitea.Token != "new-token" || st.Gitea.BaseURL != base.Gitea.BaseURL || !slices.Equal(ch[CategoryGitea], []string{FieldToken}) || ch[CategoryModel] != nil {
		t.Errorf("token: %+v %v %v", st.Gitea, ch, err)
	}
	// 清空
	if st, _, err := Apply(Overrides{}, base, Patch{Gitea: &GiteaPatch{WebhookSecret: ptr("")}}); err != nil || st.Gitea.WebhookSecret != "" {
		t.Errorf("clear: %v %v", st.Gitea, err)
	}
	for name, c := range map[string]struct {
		ov   Overrides
		p    Patch
		want error
	}{
		"config gitea field":     {Overrides{Gitea: Gitea{Token: "t"}}, Patch{Gitea: &GiteaPatch{Token: ptr("x")}}, ErrConflict},
		"config bool field":      {Overrides{Gitea: Gitea{RejectRedirects: true}}, Patch{Gitea: &GiteaPatch{RejectRedirects: ptr(false)}}, ErrConflict},
		"config model slot":      {Overrides{Main: true}, Patch{Main: &SlotPatch{Model: ptr("x")}}, ErrConflict},
		"bad url":                {Overrides{}, Patch{Gitea: &GiteaPatch{BaseURL: ptr("ftp://x"), Token: ptr("t")}}, ErrInvalid},
		"url with userinfo":      {Overrides{}, Patch{Gitea: &GiteaPatch{BaseURL: ptr("https://u:p@x"), Token: ptr("t")}}, ErrInvalid},
		"bad provider":           {Overrides{}, Patch{Main: &SlotPatch{Provider: ptr("claude")}}, ErrInvalid},
		"provider without model": {Overrides{}, Patch{Main: &SlotPatch{Model: ptr("")}}, ErrInvalid},
		"host change no token":   {Overrides{}, Patch{Gitea: &GiteaPatch{BaseURL: ptr("https://evil.example.com")}}, ErrInvalid},
		"model host no key":      {Overrides{}, Patch{Main: &SlotPatch{BaseURL: ptr("https://evil.example.com/v1")}}, ErrInvalid},
		"provider change no key": {Overrides{}, Patch{Main: &SlotPatch{Provider: ptr(ProviderDeepSeek)}}, ErrInvalid},
		"auto register missing":  {Overrides{}, Patch{Gitea: &GiteaPatch{AutoRegisterWebhook: ptr(true)}}, ErrInvalid},
	} {
		if _, _, err := Apply(c.ov, base, c.p); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// 换地址同时提交令牌：允许；同主机换路径：允许
	if _, _, err := Apply(Overrides{}, base, Patch{Gitea: &GiteaPatch{BaseURL: ptr("https://new.example.com"), Token: ptr("t2")}}); err != nil {
		t.Errorf("host change with token: %v", err)
	}
	if _, _, err := Apply(Overrides{}, base, Patch{Gitea: &GiteaPatch{BaseURL: ptr("https://git.example.com/gitea")}}); err != nil {
		t.Errorf("same host: %v", err)
	}
	// https 改成 http：协议变了也要重填令牌
	if _, _, err := Apply(Overrides{}, base, Patch{Gitea: &GiteaPatch{BaseURL: ptr("http://git.example.com")}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("scheme downgrade: %v", err)
	}
	// 两步绕过：先清空地址（一并清空令牌），再改成别处时已没有令牌可发
	st1, ch1, err := Apply(Overrides{}, base, Patch{Gitea: &GiteaPatch{BaseURL: ptr("")}})
	if err != nil || st1.Gitea.Token != "" || !slices.Contains(ch1[CategoryGitea], FieldToken) {
		t.Fatalf("clear base: %+v %v %v", st1.Gitea, ch1, err)
	}
	if st2, _, err := Apply(Overrides{}, st1, Patch{Gitea: &GiteaPatch{BaseURL: ptr("https://evil.example.com")}}); err != nil || st2.Gitea.Token != "" {
		t.Errorf("after clear: %+v %v", st2.Gitea, err)
	}
	// 令牌在代码中：首次设置地址放行；之后清空或改成别处都要改代码
	ov := Overrides{Gitea: Gitea{Token: "cfg-token"}}
	stc, _, err := Apply(ov, Stored{}, Patch{Gitea: &GiteaPatch{BaseURL: ptr("https://git.example.com")}})
	if err != nil || stc.Gitea.BaseURL == "" {
		t.Fatalf("first address with code token: %v", err)
	}
	for _, b := range []string{"", "https://evil.example.com"} {
		if _, _, err := Apply(ov, stc, Patch{Gitea: &GiteaPatch{BaseURL: ptr(b)}}); !errors.Is(err, ErrConflict) {
			t.Errorf("code token, base %q: %v", b, err)
		}
	}
	// 模型：清空槽位时一并清空 API Key
	sm, _, err := Apply(Overrides{}, base, Patch{Main: &SlotPatch{Provider: ptr(""), Model: ptr("")}})
	if err != nil || sm.Model.Main.APIKey != "" {
		t.Errorf("clear slot: %+v %v", sm.Model.Main, err)
	}
	// DeepSeek 未填模型名时使用默认值
	st, _, err = Apply(Overrides{}, Stored{}, Patch{Main: &SlotPatch{Provider: ptr(ProviderDeepSeek), APIKey: ptr("k")}})
	if err != nil || st.Model.Main.Model != DefaultDeepSeekModel {
		t.Errorf("deepseek default: %+v %v", st.Model.Main, err)
	}
	// 自动注册补齐回调地址与密钥后允许
	if _, _, err := Apply(Overrides{}, base, Patch{Gitea: &GiteaPatch{AutoRegisterWebhook: ptr(true), WebhookURL: ptr("https://app.example.com/hook")}}); err != nil {
		t.Errorf("auto register: %v", err)
	}
}

func TestMask(t *testing.T) {
	for in, want := range map[string]string{"": "", "short": "****", "12345678901": "****", "abcdefghijkl": "****", "1234567890123456789": "****", "sk-1234567890abcdefgh": "sk-****efgh"} {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}
