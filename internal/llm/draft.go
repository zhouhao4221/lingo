package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zhouhao4221/lingo/internal/textutil"
)

// Message 是对话消息，与 lingo.Message 对应（由根包转换）。
type Message struct {
	Role    string // system | user | assistant
	Content string
}

// Chat 是对话模型，由根包把 lingo.Model 适配而来。
type Chat func(ctx context.Context, messages []Message) (string, error)

// FieldSpec 是 Issue 表单模板中的一个输入字段。
type FieldSpec struct {
	ID          string
	Label       string
	Kind        string // input | textarea | dropdown | checkboxes
	Description string
	Required    bool
	Options     []string // dropdown / checkboxes 的选项
	Multiple    bool     // dropdown 是否可多选
}

// DraftInput 是待整理的工单内容。不包含提交人身份信息。
type DraftInput struct {
	Type        string // consult | bug | feature
	Title       string
	Body        string
	Fields      map[string][]string // 提交人已填的模板字段
	Context     map[string]string   // 页面、浏览器、版本等环境
	Version     string
	TitlePrefix string      // 模板的标题前缀，由调用方拼接，模型不必重复
	Template    []FieldSpec // 仓库 Issue 表单模板的字段；为空表示没有表单模板
}

// Draft 是整理后的 Issue 内容。
type Draft struct {
	Title       string
	Description string              // 整理后的问题描述
	Fields      map[string][]string // 合并后的模板字段：提交人已填的保持原值，模型只补空缺
}

// Drafter 用轻量模型把工单整理为结构清晰的 Issue：生成标题、从自由描述中抽取模板字段、整理描述。
// 输出逐项校验（字段 ID、选项须来自模板），不合格的丢弃；提交人已填的字段不改。
type Drafter struct {
	Chat Chat
}

const (
	maxInputRunes = 8000 // 送给模型的正文上限
	maxTitleRunes = 120
	maxFieldRunes = 4000
)

const draftSystemPrompt = `你是软件研发团队的工单整理助手。用户提交的反馈会被转成 Gitea Issue 交给研发处理。
你的任务是把反馈整理清楚，不是回答问题，也不是给出解决方案。

规则：
1. 只依据用户提供的内容整理，绝不编造现象、步骤、版本、环境等事实；内容里没有的就留空。
2. 使用用户的语言（中文反馈用中文）。
3. title：一句话概括问题，具体、可检索，不超过 60 个字，不要加类型前缀或标点结尾。
4. description：把用户的描述整理成条理清楚的 Markdown（可用列表），保留所有有用的细节、报错原文与数字，去掉寒暄与重复。
5. fields：只能使用给定模板中的字段 id；dropdown / checkboxes 的值只能从该字段的 options 中原样选择；
   能从用户内容中明确得到的才填写，用户已填写的字段不要输出。
6. 不要输出任何个人信息（姓名、手机号、邮箱、账号、地址）；用户内容中出现的个人信息不要写进结果。
7. 只输出一个 JSON 对象，不要输出其他文字：{"title": "...", "description": "...", "fields": {"字段id": ["值"]}}`

// DraftIssue 调用模型整理工单；模型出错或输出不合格时返回错误，调用方应退回原内容。
func (d *Drafter) DraftIssue(ctx context.Context, in DraftInput) (*Draft, error) {
	if d == nil || d.Chat == nil {
		return nil, errors.New("llm: no model")
	}
	prompt, err := draftPrompt(in)
	if err != nil {
		return nil, err
	}
	out, err := d.Chat(ctx, []Message{{Role: "system", Content: draftSystemPrompt}, {Role: "user", Content: prompt}})
	if err != nil {
		return nil, fmt.Errorf("llm: chat: %w", err)
	}
	return parseDraft(out, in)
}

func draftPrompt(in DraftInput) (string, error) {
	type field struct {
		ID          string   `json:"id"`
		Label       string   `json:"label"`
		Kind        string   `json:"kind"`
		Description string   `json:"description,omitempty"`
		Required    bool     `json:"required,omitempty"`
		Options     []string `json:"options,omitempty"`
		Multiple    bool     `json:"multiple,omitempty"`
	}
	fields := make([]field, 0, len(in.Template))
	for _, f := range in.Template {
		fields = append(fields, field{f.ID, f.Label, f.Kind, f.Description, f.Required, f.Options, f.Multiple})
	}
	b, err := json.MarshalIndent(map[string]any{
		"type":            in.Type,
		"title":           textutil.Clip(in.Title, maxTitleRunes*2),
		"content":         textutil.Clip(in.Body, maxInputRunes),
		"filled_fields":   in.Fields,
		"environment":     in.Context,
		"version":         in.Version,
		"template_fields": fields,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return "请整理下面的用户反馈（JSON）：\n\n" + string(b), nil
}

// parseDraft 解析并校验模型输出。
func parseDraft(out string, in DraftInput) (*Draft, error) {
	type rawDraft struct {
		Title       string         `json:"title"`
		Description string         `json:"description"`
		Fields      map[string]any `json:"fields"`
	}
	// 模型可能在 JSON 前后加说明或代码围栏：从每个 "{" 起尝试解码，取第一个含 title 或 description 的对象
	var raw rawDraft
	found := false
	for i := strings.IndexByte(out, '{'); i >= 0; {
		var r rawDraft
		if err := json.NewDecoder(strings.NewReader(out[i:])).Decode(&r); err == nil && (r.Title != "" || r.Description != "") {
			raw, found = r, true
			break
		}
		next := strings.IndexByte(out[i+1:], '{')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	if !found {
		return nil, errors.New("llm: draft is not JSON")
	}
	d := &Draft{Title: oneLine(raw.Title), Description: strings.TrimSpace(raw.Description), Fields: map[string][]string{}}
	if d.Title == "" || utf8.RuneCountInString(d.Title) > maxTitleRunes {
		d.Title = in.Title // 标题缺失或过长：保留原标题
	}
	d.Description = textutil.Clip(d.Description, maxInputRunes)
	for id, vs := range in.Fields {
		d.Fields[id] = vs // 提交人已填的字段原样保留
	}
	specs := map[string]FieldSpec{}
	for _, f := range in.Template {
		specs[f.ID] = f
	}
	for id, v := range raw.Fields {
		spec, ok := specs[id]
		if !ok || textutil.HasValue(in.Fields[id]) {
			continue // 模板中没有的字段、提交人已填的字段：不采纳
		}
		if vals := validValues(spec, v); len(vals) > 0 {
			d.Fields[id] = vals
		}
	}
	return d, nil
}

// validValues 校验模型给出的字段值：选项类只接受模板中的选项（原样），文本类取非空字符串。
func validValues(spec FieldSpec, v any) []string {
	var vals []string
	switch x := v.(type) {
	case string:
		vals = []string{x}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				vals = append(vals, s)
			}
		}
	}
	var out []string
	for _, s := range vals {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		switch spec.Kind {
		case "dropdown", "checkboxes":
			if slices.Contains(spec.Options, s) && !slices.Contains(out, s) {
				out = append(out, s)
			}
		case "input":
			out = append(out, textutil.Clip(strings.Join(strings.Fields(s), " "), maxFieldRunes)) // 单行字段
		default:
			out = append(out, textutil.Clip(s, maxFieldRunes))
		}
	}
	if spec.Kind == "dropdown" && !spec.Multiple && len(out) > 1 {
		out = out[:1]
	}
	switch {
	case spec.Kind == "input" && len(out) > 1:
		out = []string{strings.Join(out, "; ")}
	case spec.Kind == "textarea" && len(out) > 1:
		out = []string{strings.Join(out, "\n")}
	}
	return out
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}
