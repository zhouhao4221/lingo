package gitea

import (
	"encoding/json"
	"testing"
)

// bugTemplate 模拟 GET /issue_templates 返回的 YAML 表单模板。
const bugTemplateJSON = `{
  "name": "Bug 报告", "title": "[Bug] ", "about": "报告问题", "labels": ["type/bug"], "file_name": "bug.yaml",
  "body": [
    {"type": "markdown", "attributes": {"value": "感谢反馈！"}},
    {"type": "input", "id": "version", "attributes": {"label": "版本"}, "validations": {"required": true}},
    {"type": "textarea", "id": "steps", "attributes": {"label": "复现步骤"}, "validations": {"required": true}},
    {"type": "textarea", "id": "logs", "attributes": {"label": "日志", "render": "shell"}},
    {"type": "dropdown", "id": "browser", "attributes": {"label": "浏览器", "options": ["Chrome", "Firefox", "Safari"], "multiple": true}},
    {"type": "dropdown", "id": "os", "attributes": {"label": "系统", "options": ["Linux", "macOS"], "list": true}},
    {"type": "checkboxes", "id": "confirm", "attributes": {"label": "确认", "options": [
      {"label": "已搜索过类似问题", "required": true},
      {"label": "仅表单可见", "visible": ["form"]}
    ]}},
    {"type": "input", "id": "hidden", "attributes": {"label": "内部"}, "visible": ["form"]},
    {"type": "input", "id": "contact", "attributes": {"label": "联系方式", "hide_label": true}}
  ]
}`

func loadBugTemplate(t *testing.T) *IssueTemplate {
	t.Helper()
	var tpl IssueTemplate
	if err := json.Unmarshal([]byte(bugTemplateJSON), &tpl); err != nil {
		t.Fatal(err)
	}
	return &tpl
}

func TestRenderFormMatchesGitea(t *testing.T) {
	tpl := loadBugTemplate(t)
	if !tpl.IsForm() || tpl.Labels[0] != "type/bug" {
		t.Fatalf("template = %+v", tpl)
	}
	got := RenderForm(tpl, FormValues{
		"version": {" 1.2.3 "},
		"steps":   {"1. 打开页面\n2. 点击导出"},
		"logs":    {"panic: x\n````inner"},
		"browser": {"Firefox", "Chrome"},
		"os":      {"macOS"},
		"confirm": {"已搜索过类似问题"},
		"hidden":  {"不应出现"},
		"contact": {"工单内联系"},
	})
	// 期望值按 Gitea modules/issue/template.RenderToMarkdown 的规则手工推导：
	// markdown 默认不进正文；选项按模板顺序输出；textarea 有 render 时包代码块，围栏比内容中最长的多一个反引号。
	want := "### 版本\n\n1.2.3\n\n" +
		"### 复现步骤\n\n1. 打开页面\n2. 点击导出\n\n" +
		"### 日志\n\n`````shell\npanic: x\n````inner\n`````\n\n" +
		"### 浏览器\n\nChrome, Firefox\n\n" +
		"### 系统\n\n- macOS\n\n" +
		"### 确认\n\n- [x] 已搜索过类似问题\n\n" +
		"工单内联系\n\n"
	if got != want {
		t.Errorf("RenderForm mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderFormBlanks(t *testing.T) {
	got := RenderForm(loadBugTemplate(t), nil)
	want := "### 版本\n\n_No response_\n\n" +
		"### 复现步骤\n\n_No response_\n\n" +
		"### 日志\n\n_No response_\n\n" +
		"### 浏览器\n\n_No response_\n\n" +
		"### 系统\n\n_No response_\n\n" +
		"### 确认\n\n- [ ] 已搜索过类似问题\n\n" +
		"_No response_\n\n"
	if got != want {
		t.Errorf("blank render\n got: %q\nwant: %q", got, want)
	}
}

func TestMissingRequired(t *testing.T) {
	tpl := loadBugTemplate(t)
	miss := MissingRequired(tpl, FormValues{"version": {"1.0"}, "steps": {"  "}})
	if len(miss) != 1 || miss[0].ID != "steps" || miss[0].Label() != "复现步骤" {
		t.Errorf("missing = %+v", miss)
	}
	if f := tpl.Fields[4]; !f.Multiple() || len(f.Options()) != 3 {
		t.Errorf("dropdown helpers: multiple=%v options=%v", f.Multiple(), f.Options())
	}
	if opts := tpl.Fields[6].Options(); len(opts) != 2 || opts[0] != "已搜索过类似问题" {
		t.Errorf("checkbox options = %v", opts)
	}
}

func TestStringListAcceptsStringOrArray(t *testing.T) {
	var a, b StringList
	if err := json.Unmarshal([]byte(`["x","y"]`), &a); err != nil || len(a) != 2 {
		t.Errorf("array: %v %v", a, err)
	}
	if err := json.Unmarshal([]byte(`"x, y,,z"`), &b); err != nil || len(b) != 3 || b[2] != "z" {
		t.Errorf("string: %v %v", b, err)
	}
}
