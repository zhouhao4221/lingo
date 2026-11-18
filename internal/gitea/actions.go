package gitea

import (
	"context"
	"net/url"
	"strings"
)

// 以下接口需 Gitea ≥ 1.25；更低版本返回 404。

// ListWorkflows 返回仓库的 Actions 工作流。
func (c *Client) ListWorkflows(ctx context.Context, owner, repo string) ([]*Workflow, error) {
	var r struct {
		Workflows []*Workflow `json:"workflows"`
	}
	_, err := c.do(ctx, "GET", repoPath(owner, repo)+"/actions/workflows", nil, nil, &r)
	return r.Workflows, err
}

// ListRunsOptions 是列出工作流运行的条件。
type ListRunsOptions struct {
	ListOptions
	Status  string // 如 "success"、"completed"
	Event   string
	Branch  string
	HeadSHA string
}

// ListWorkflowRuns 返回仓库工作流运行的一页（按时间倒序）。
func (c *Client) ListWorkflowRuns(ctx context.Context, owner, repo string, o ListRunsOptions) ([]*WorkflowRun, *Response, error) {
	q := url.Values{}
	o.ListOptions.apply(q)
	for k, v := range map[string]string{"status": o.Status, "event": o.Event, "branch": o.Branch, "head_sha": o.HeadSHA} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var r struct {
		WorkflowRuns []*WorkflowRun `json:"workflow_runs"`
	}
	resp, err := c.do(ctx, "GET", repoPath(owner, repo)+"/actions/runs", q, nil, &r)
	return r.WorkflowRuns, resp, err
}

// WorkflowFile 返回运行所属工作流的文件名（去掉目录与 @ref），如 "deploy-prod.yml"，
// 用于与 Repo.Deploy.Workflow 比对。
func (r *WorkflowRun) WorkflowFile() string {
	p := r.Path
	if i := strings.IndexByte(p, '@'); i >= 0 {
		p = p[:i]
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	return p
}
