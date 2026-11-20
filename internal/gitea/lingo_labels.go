package gitea

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// LingoLabels 返回灵问使用的标签（前缀默认 lingo）：来源标签，以及 type/*、review/* 两组互斥作用域标签。
func LingoLabels(prefix string) []CreateLabelOption {
	if prefix == "" {
		prefix = "lingo"
	}
	out := []CreateLabelOption{{Name: prefix, Color: "#6f42c1", Description: "由灵问创建"}}
	for _, t := range []struct{ name, color string }{{"consult", "#1d76db"}, {"bug", "#d73a4a"}, {"feature", "#0e8a16"}} {
		out = append(out, TypeLabel(prefix, t.name, t.color))
	}
	for _, r := range []struct{ name, color string }{{"pending", "#fbca04"}, {"approved", "#0e8a16"}, {"rejected", "#b60205"}, {"need-info", "#d93f0b"}} {
		out = append(out, CreateLabelOption{Name: prefix + "/review/" + r.name, Color: r.color, Exclusive: true})
	}
	return out
}

// TypeLabel 返回工单类型标签（互斥作用域）；color 为空时取默认色。
func TypeLabel(prefix, typ, color string) CreateLabelOption {
	if color == "" {
		color = "#c5def5"
		for _, l := range LingoLabels(prefix) {
			if l.Name == prefix+"/type/"+typ {
				return l
			}
		}
	}
	return CreateLabelOption{Name: prefix + "/type/" + typ, Color: color, Exclusive: true}
}

// LabelLister 是 EnsureLabels 用到的接口，由 Client 实现。
type LabelLister interface {
	ListLabels(ctx context.Context, owner, repo string, o ListOptions) ([]*Label, *Response, error)
	CreateLabel(ctx context.Context, owner, repo string, opt CreateLabelOption) (*Label, error)
}

// EnsureLabels 列出仓库标签并创建 want 中缺少的（按名称不区分大小写），返回全部标签 name（小写）→ id 与新建的标签名。
// 单个标签创建失败不影响其他标签，错误合并返回。
func EnsureLabels(ctx context.Context, g LabelLister, owner, repo string, want []CreateLabelOption) (map[string]int64, []string, error) {
	ls, err := ListAll(ctx, 50, func(ctx context.Context, o ListOptions) ([]*Label, *Response, error) {
		return g.ListLabels(ctx, owner, repo, o)
	})
	if err != nil {
		return nil, nil, err
	}
	ids := map[string]int64{}
	for _, l := range ls {
		ids[strings.ToLower(l.Name)] = l.ID
	}
	var created []string
	var errs []error
	for _, spec := range want {
		if _, ok := ids[strings.ToLower(spec.Name)]; ok {
			continue
		}
		l, err := g.CreateLabel(ctx, owner, repo, spec)
		if err != nil {
			errs = append(errs, fmt.Errorf("create label %s: %w", spec.Name, err))
			continue
		}
		ids[strings.ToLower(spec.Name)] = l.ID
		created = append(created, spec.Name)
	}
	return ids, created, errors.Join(errs...)
}

// WorkflowMatches 报告工作流路径（如 ".gitea/workflows/deploy.yml@refs/heads/main"）是否为配置的文件 file
// （文件名或完整路径均可）。
func WorkflowMatches(workflowPath, file string) bool {
	p, _, _ := strings.Cut(workflowPath, "@")
	return p != "" && file != "" && (path.Base(p) == file || p == file)
}

// HookEvents 返回要订阅的 Webhook 事件；actions 为假（Gitea < 1.25）时不含 workflow_run。
func HookEvents(actions bool) []string {
	if actions {
		return slices.Clone(LingoHookEvents)
	}
	return slices.DeleteFunc(slices.Clone(LingoHookEvents), func(e string) bool { return e == "workflow_run" })
}
