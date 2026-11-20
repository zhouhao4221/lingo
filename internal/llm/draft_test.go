package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var tmpl = []FieldSpec{
	{ID: "what", Label: "发生了什么", Kind: "textarea", Required: true},
	{ID: "steps", Label: "复现步骤", Kind: "textarea"},
	{ID: "browser", Label: "浏览器", Kind: "dropdown", Options: []string{"Chrome", "Safari", "Firefox"}},
	{ID: "os", Label: "系统", Kind: "checkboxes", Options: []string{"macOS", "Windows"}},
}

func drafter(out string, err error, got *[]Message) *Drafter {
	return &Drafter{Chat: func(_ context.Context, ms []Message) (string, Usage, error) {
		if got != nil {
			*got = ms
		}
		return out, Usage{Model: "m", Input: 100, Output: 20, Cached: 60}, err
	}}
}

func TestDraftIssueValidates(t *testing.T) {
	in := DraftInput{Type: "bug", Title: "登录不了", Body: "用 Safari 登录后白屏，先点登录再输密码", Template: tmpl,
		Fields: map[string][]string{"what": {"白屏"}}}
	var msgs []Message
	out := "好的：\n```json\n" + `{"title":"Safari 登录后页面白屏","description":"- 登录后白屏","fields":{
		"what":["模型改写的内容"],"steps":["1. 点登录","2. 输密码"],"browser":["Safari","Chrome"],"os":["Linux"],"unknown":["x"]}}` + "\n```"
	d, _, err := drafter(out, nil, &msgs).DraftIssue(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Safari 登录后页面白屏" || d.Description != "- 登录后白屏" {
		t.Errorf("draft = %+v", d)
	}
	// 已填的 what 不改；steps 合并为一段；browser 单选取第一个合法值；os 非法选项丢弃；未知字段丢弃
	if d.Fields["what"][0] != "白屏" || d.Fields["steps"][0] != "1. 点登录\n2. 输密码" || len(d.Fields["browser"]) != 1 ||
		d.Fields["browser"][0] != "Safari" || d.Fields["os"] != nil || d.Fields["unknown"] != nil {
		t.Errorf("fields = %v", d.Fields)
	}
	// 模板定义在前、工单内容在后，同一仓库的请求前缀相同，便于服务端前缀缓存命中
	if p := msgs[1].Content; len(msgs) != 2 || msgs[0].Role != "system" || !strings.Contains(p, "template_fields") ||
		strings.Index(p, "template_fields") > strings.Index(p, "登录后白屏") {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestDraftIssueProseAroundJSON(t *testing.T) {
	in := DraftInput{Title: "t", Template: []FieldSpec{{ID: "ver", Kind: "input"}}}
	out := "结果 {ticket} 如下：\n" + `{"title":"版本 1.2 导出失败","description":"d","fields":{"ver":["1.2","1.3\n beta"]}}` + "\n谢谢 {end}"
	d, _, err := drafter(out, nil, nil).DraftIssue(context.Background(), in)
	if err != nil || d.Title != "版本 1.2 导出失败" || d.Fields["ver"][0] != "1.2; 1.3 beta" {
		t.Errorf("draft = %+v %v", d, err)
	}
}

func TestDraftIssueFailures(t *testing.T) {
	in := DraftInput{Title: "原标题", Body: "x"}
	if _, _, err := drafter("", errors.New("timeout"), nil).DraftIssue(context.Background(), in); err == nil {
		t.Error("model error not returned")
	}
	if _, _, err := drafter("抱歉，我无法处理", nil, nil).DraftIssue(context.Background(), in); err == nil {
		t.Error("non-JSON accepted")
	}
	d, _, err := drafter(`{"title":"","description":"整理后"}`, nil, nil).DraftIssue(context.Background(), in)
	if err != nil || d.Title != "原标题" {
		t.Errorf("empty title: %+v %v", d, err)
	}
	if _, _, err := (*Drafter)(nil).DraftIssue(context.Background(), in); err == nil {
		t.Error("nil drafter")
	}
}
