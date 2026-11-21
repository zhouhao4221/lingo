package gitea

import (
	"context"
	"encoding/json"
	"strings"
)

// Issue 表单字段类型。
const (
	FieldMarkdown   = "markdown"
	FieldTextarea   = "textarea"
	FieldInput      = "input"
	FieldDropdown   = "dropdown"
	FieldCheckboxes = "checkboxes"
)

// IssueTemplate 是仓库 .gitea/ISSUE_TEMPLATE/（兼容 .github/）下的模板，Gitea 已解析好。
// Markdown 模板只有 Content；YAML 表单模板有 Fields。
type IssueTemplate struct {
	Name      string            `json:"name"`
	Title     string            `json:"title"` // 标题前缀
	About     string            `json:"about"`
	Labels    StringList        `json:"labels"`
	Assignees StringList        `json:"assignees"`
	Ref       string            `json:"ref"`
	Content   string            `json:"content"`
	Fields    []*IssueFormField `json:"body"`
	FileName  string            `json:"file_name"`
}

// IsForm 报告是否为 YAML 表单模板。
func (t *IssueTemplate) IsForm() bool { return len(t.Fields) > 0 }

// StringList 兼容 Gitea 把模板 labels / assignees 输出为数组或逗号分隔字符串两种形式。
type StringList []string

// UnmarshalJSON 实现 json.Unmarshaler。
func (s *StringList) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		*s = arr
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	*s = nil
	for _, v := range strings.Split(one, ",") {
		if v = strings.TrimSpace(v); v != "" {
			*s = append(*s, v)
		}
	}
	return nil
}

// IssueFormField 是表单模板中的一个字段。
type IssueFormField struct {
	Type        string         `json:"type"`
	ID          string         `json:"id"`
	Attributes  map[string]any `json:"attributes"`
	Validations map[string]any `json:"validations"`
	Visible     []string       `json:"visible"` // "form" / "content"，为空时 markdown 只在表单显示，其余两处都显示
}

// Label 返回字段标题。
func (f *IssueFormField) Label() string { s, _ := f.Attributes["label"].(string); return s }

// Description 返回字段说明。
func (f *IssueFormField) Description() string {
	s, _ := f.Attributes["description"].(string)
	return s
}

// Placeholder 返回输入提示。
func (f *IssueFormField) Placeholder() string {
	s, _ := f.Attributes["placeholder"].(string)
	return s
}

// Required 报告字段是否必填。
func (f *IssueFormField) Required() bool { b, _ := f.Validations["required"].(bool); return b }

// Multiple 报告 dropdown 是否允许多选。
func (f *IssueFormField) Multiple() bool { b, _ := f.Attributes["multiple"].(bool); return b }

// Options 返回 dropdown / checkboxes 的选项标题。
func (f *IssueFormField) Options() []string {
	raw, _ := f.Attributes["options"].([]any)
	out := make([]string, 0, len(raw))
	for _, o := range raw {
		switch v := o.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			s, _ := v["label"].(string)
			out = append(out, s)
		}
	}
	return out
}

// RequiredOptions 返回 checkboxes 中标记 required 的选项标题。
func (f *IssueFormField) RequiredOptions() []string {
	raw, _ := f.Attributes["options"].([]any)
	var out []string
	for _, o := range raw {
		if m, ok := o.(map[string]any); ok && isTrue(m["required"]) {
			s, _ := m["label"].(string)
			out = append(out, s)
		}
	}
	return out
}

// IsInput 报告字段是否需要用户填写（markdown 只是说明文字）。
func (f *IssueFormField) IsInput() bool { return f.Type != FieldMarkdown }

func (f *IssueFormField) visibleInContent() bool {
	if len(f.Visible) == 0 {
		return f.Type != FieldMarkdown
	}
	for _, v := range f.Visible {
		if v == "content" {
			return true
		}
	}
	return false
}

func (f *IssueFormField) hideLabel() bool {
	if f.Type == FieldMarkdown {
		return true
	}
	b, _ := f.Attributes["hide_label"].(bool)
	return b
}

// optionVisibleInContent 对应 checkboxes 选项的 visible 设置。
func (f *IssueFormField) optionVisibleInContent(i int) bool {
	raw, _ := f.Attributes["options"].([]any)
	if i >= len(raw) {
		return true
	}
	m, ok := raw[i].(map[string]any)
	if !ok {
		return true
	}
	vl, ok := m["visible"].([]any)
	if !ok {
		return true
	}
	for _, v := range vl {
		if s, _ := v.(string); s == "content" {
			return true
		}
	}
	return false
}

// MatchTemplate 选出工单类型 typ 对应的模板：先按文件名 file（如 "bug_report.yaml"，可空），
// 再按模板标签 "type/<typ>"、"<prefix>/type/<typ>" 或 "<typ>"（不区分大小写）；都没有时返回 nil。
func MatchTemplate(ts []*IssueTemplate, file, typ, prefix string) *IssueTemplate {
	if file != "" {
		for _, t := range ts {
			if t.FileName == file || strings.HasSuffix(t.FileName, "/"+file) {
				return t
			}
		}
	}
	want := []string{"type/" + typ, prefix + "/type/" + typ, typ}
	for _, t := range ts {
		for _, l := range t.Labels {
			for _, w := range want {
				if strings.EqualFold(w, l) {
					return t
				}
			}
		}
	}
	return nil
}

// FormValues 是表单取值，按字段 ID：input / textarea 取第一个值；
// dropdown 为选中的选项标题；checkboxes 为勾选的选项标题。
type FormValues map[string][]string

// RenderForm 按 Gitea 网页提交表单时的格式（modules/issue/template.RenderToMarkdown）渲染 Issue 正文，
// 使灵工建的 Issue 与研发在网页上按同一模板提交的一致。
func RenderForm(t *IssueTemplate, v FormValues) string {
	const blank = "_No response_\n"
	var b strings.Builder
	for _, f := range t.Fields {
		if f.ID == "" || !f.visibleInContent() {
			continue
		}
		if !f.hideLabel() {
			b.WriteString("### " + f.Label() + "\n\n")
		}
		vals := v[f.ID]
		first := ""
		if len(vals) > 0 {
			first = strings.TrimSpace(vals[0])
		}
		switch f.Type {
		case FieldCheckboxes:
			for i, opt := range f.Options() {
				if !f.optionVisibleInContent(i) {
					continue
				}
				mark := " "
				if contains(vals, opt) {
					mark = "x"
				}
				b.WriteString("- [" + mark + "] " + opt + "\n")
			}
		case FieldDropdown:
			var picked []string
			for _, opt := range f.Options() {
				if contains(vals, opt) {
					picked = append(picked, opt)
				}
			}
			switch {
			case len(picked) == 0:
				b.WriteString(blank)
			case isTrue(f.Attributes["list"]):
				for _, p := range picked {
					b.WriteString("- " + p + "\n")
				}
			default:
				b.WriteString(strings.Join(picked, ", ") + "\n")
			}
		case FieldInput:
			if first == "" {
				b.WriteString(blank)
			} else {
				b.WriteString(first + "\n")
			}
		case FieldTextarea:
			render, _ := f.Attributes["render"].(string)
			switch {
			case first == "":
				b.WriteString(blank)
			case render != "":
				q := minQuotes(first)
				b.WriteString(q + render + "\n" + first + "\n" + q + "\n")
			default:
				b.WriteString(first + "\n")
			}
		case FieldMarkdown:
			if s, ok := f.Attributes["value"].(string); ok {
				b.WriteString(s + "\n")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// MissingRequired 返回必填但未填写的字段（供问答 Agent 追问）。
// checkboxes 的必填在各选项上（options[].required），任一必选项未勾选即视为缺失。
func MissingRequired(t *IssueTemplate, v FormValues) []*IssueFormField {
	var out []*IssueFormField
	for _, f := range t.Fields {
		if f.Type == FieldCheckboxes {
			for _, opt := range f.RequiredOptions() {
				if !contains(v[f.ID], opt) {
					out = append(out, f)
					break
				}
			}
			continue
		}
		if !f.IsInput() || !f.Required() {
			continue
		}
		filled := false
		for _, s := range v[f.ID] {
			if strings.TrimSpace(s) != "" {
				filled = true
				break
			}
		}
		if !filled {
			out = append(out, f)
		}
	}
	return out
}

// minQuotes 返回足以包住 value 的代码块围栏：至少三个反引号，且比 value 中任一行行首（允许缩进至多
// 3 个空格，按 CommonMark 这样的行仍能关闭代码块）最长的反引号串多一个。
// Gitea 只检查整个值的开头，这里有意更严格，避免值中的围栏提前关闭代码块；对不含围栏的值两者结果一致。
func minQuotes(value string) string {
	n := 3
	for _, line := range strings.Split(value, "\n") {
		for i := 0; i < 3 && strings.HasPrefix(line, " "); i++ {
			line = line[1:]
		}
		k := 0
		for k < len(line) && line[k] == '`' {
			k++
		}
		if k >= 3 && k+1 > n {
			n = k + 1
		}
	}
	return strings.Repeat("`", n)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func isTrue(v any) bool { b, _ := v.(bool); return b }

// IssueConfig 是仓库 Issue 配置（.gitea/ISSUE_TEMPLATE/config.yaml）。
type IssueConfig struct {
	BlankIssuesEnabled bool `json:"blank_issues_enabled"`
	ContactLinks       []struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		About string `json:"about"`
	} `json:"contact_links"`
}

// IssueConfigValidation 是 Issue 配置的校验结果。
type IssueConfigValidation struct {
	Valid   bool   `json:"valid"`
	Message string `json:"message"`
}

// ListIssueTemplates 返回仓库可用的 Issue 模板。
func (c *Client) ListIssueTemplates(ctx context.Context, owner, repo string) ([]*IssueTemplate, error) {
	var ts []*IssueTemplate
	_, err := c.do(ctx, "GET", repoPath(owner, repo)+"/issue_templates", nil, nil, &ts)
	return ts, err
}

// GetIssueConfig 返回仓库 Issue 配置。
func (c *Client) GetIssueConfig(ctx context.Context, owner, repo string) (*IssueConfig, error) {
	var ic IssueConfig
	_, err := c.do(ctx, "GET", repoPath(owner, repo)+"/issue_config", nil, nil, &ic)
	return &ic, err
}

// ValidateIssueConfig 校验仓库 Issue 配置。
func (c *Client) ValidateIssueConfig(ctx context.Context, owner, repo string) (*IssueConfigValidation, error) {
	var v IssueConfigValidation
	_, err := c.do(ctx, "GET", repoPath(owner, repo)+"/issue_config/validate", nil, nil, &v)
	return &v, err
}
