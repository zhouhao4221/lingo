package gitea

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func issuePath(owner, repo string, index int64) string {
	return repoPath(owner, repo) + "/issues/" + strconv.FormatInt(index, 10)
}

// CreateIssueOption 是建 Issue 的参数；Labels 为标签 id。
type CreateIssueOption struct {
	Title     string     `json:"title"`
	Body      string     `json:"body,omitempty"`
	Labels    []int64    `json:"labels,omitempty"`
	Assignees []string   `json:"assignees,omitempty"`
	Milestone int64      `json:"milestone,omitempty"`
	Ref       string     `json:"ref,omitempty"`
	DueDate   *time.Time `json:"due_date,omitempty"`
}

// EditIssueOption 是编辑 Issue 的参数，nil 字段不修改。
type EditIssueOption struct {
	Title        *string    `json:"title,omitempty"`
	Body         *string    `json:"body,omitempty"`
	State        *string    `json:"state,omitempty"`     // StateOpen | StateClosed
	Assignees    *[]string  `json:"assignees,omitempty"` // 指向空切片表示清空指派人
	Milestone    *int64     `json:"milestone,omitempty"`
	DueDate      *time.Time `json:"due_date,omitempty"`
	UnsetDueDate bool       `json:"unset_due_date,omitempty"`
}

// CreateIssue 在仓库中创建 Issue。
func (c *Client) CreateIssue(ctx context.Context, owner, repo string, opt CreateIssueOption) (*Issue, error) {
	var is Issue
	_, err := c.do(ctx, "POST", repoPath(owner, repo)+"/issues", nil, opt, &is)
	return &is, err
}

// GetIssue 返回 Issue；不存在时 errors.Is(err, ErrNotFound)。
func (c *Client) GetIssue(ctx context.Context, owner, repo string, index int64) (*Issue, error) {
	var is Issue
	_, err := c.do(ctx, "GET", issuePath(owner, repo, index), nil, nil, &is)
	return &is, err
}

// EditIssue 修改 Issue 标题、正文、状态等。
func (c *Client) EditIssue(ctx context.Context, owner, repo string, index int64, opt EditIssueOption) (*Issue, error) {
	var is Issue
	_, err := c.do(ctx, "PATCH", issuePath(owner, repo, index), nil, opt, &is)
	return &is, err
}

// ListIssuesOptions 是列出仓库 Issue 的过滤条件。
type ListIssuesOptions struct {
	ListOptions
	State  string    // StateOpen（默认）| StateClosed | StateAll
	Labels []string  // 标签名，同时具备
	Query  string    // 关键词
	Since  time.Time // 只返回此后更新的（对账增量）
	Before time.Time
	// PullRequests 为假时只返回 Issue（type=issues），为真时只返回 PR。
	PullRequests bool
}

func (o ListIssuesOptions) values() url.Values {
	q := url.Values{}
	o.ListOptions.apply(q)
	if o.State != "" {
		q.Set("state", o.State)
	}
	if len(o.Labels) > 0 {
		q.Set("labels", strings.Join(o.Labels, ","))
	}
	if o.Query != "" {
		q.Set("q", o.Query)
	}
	if !o.Since.IsZero() {
		q.Set("since", o.Since.UTC().Format(time.RFC3339))
	}
	if !o.Before.IsZero() {
		q.Set("before", o.Before.UTC().Format(time.RFC3339))
	}
	if o.PullRequests {
		q.Set("type", "pulls")
	} else {
		q.Set("type", "issues")
	}
	return q
}

// ListIssues 返回仓库 Issue 的一页。
func (c *Client) ListIssues(ctx context.Context, owner, repo string, o ListIssuesOptions) ([]*Issue, *Response, error) {
	var is []*Issue
	resp, err := c.do(ctx, "GET", repoPath(owner, repo)+"/issues", o.values(), nil, &is)
	return is, resp, err
}

// SearchIssuesOptions 是跨仓库搜索 Issue 的条件。
type SearchIssuesOptions struct {
	ListOptions
	Query  string
	State  string
	Labels []string
	Owner  string // 限定仓库所有者（用户或组织）
	Since  time.Time
}

// SearchIssues 跨仓库搜索当前令牌可见的 Issue（提单前查重、找相似问题）。
func (c *Client) SearchIssues(ctx context.Context, o SearchIssuesOptions) ([]*Issue, *Response, error) {
	q := url.Values{}
	o.ListOptions.apply(q)
	q.Set("type", "issues")
	if o.Query != "" {
		q.Set("q", o.Query)
	}
	if o.State != "" {
		q.Set("state", o.State)
	}
	if len(o.Labels) > 0 {
		q.Set("labels", strings.Join(o.Labels, ","))
	}
	if o.Owner != "" {
		q.Set("owner", o.Owner)
	}
	if !o.Since.IsZero() {
		q.Set("since", o.Since.UTC().Format(time.RFC3339))
	}
	var is []*Issue
	resp, err := c.do(ctx, "GET", "/repos/issues/search", q, nil, &is)
	return is, resp, err
}

// AddAssignees 给 Issue 追加指派人。
func (c *Client) AddAssignees(ctx context.Context, owner, repo string, index int64, logins []string) (*Issue, error) {
	var is Issue
	_, err := c.do(ctx, "POST", issuePath(owner, repo, index)+"/assignees", nil, map[string][]string{"assignees": logins}, &is)
	return &is, err
}

// LockIssue 锁定 Issue，阻止非协作者继续评论。
func (c *Client) LockIssue(ctx context.Context, owner, repo string, index int64, reason string) error {
	_, err := c.do(ctx, "PUT", issuePath(owner, repo, index)+"/lock", nil, map[string]string{"lock_reason": reason}, nil)
	return err
}

// UnlockIssue 解锁 Issue。
func (c *Client) UnlockIssue(ctx context.Context, owner, repo string, index int64) error {
	_, err := c.do(ctx, "DELETE", issuePath(owner, repo, index)+"/lock", nil, nil, nil)
	return err
}

// AddDependency 让 Issue index 依赖于 dep（dep 关闭前 index 被阻塞），可跨仓库；
// 需仓库启用 internal_tracker.enable_issue_dependencies。
func (c *Client) AddDependency(ctx context.Context, owner, repo string, index int64, dep IssueMeta) error {
	_, err := c.do(ctx, "POST", issuePath(owner, repo, index)+"/dependencies", nil, dep, nil)
	return err
}

// ListDependencies 返回阻塞 Issue index 的 Issue。
func (c *Client) ListDependencies(ctx context.Context, owner, repo string, index int64, o ListOptions) ([]*Issue, *Response, error) {
	q := url.Values{}
	o.apply(q)
	var is []*Issue
	resp, err := c.do(ctx, "GET", issuePath(owner, repo, index)+"/dependencies", q, nil, &is)
	return is, resp, err
}

// ListTimeline 返回 Issue 的 timeline（评论、标签、指派、关闭、被提交 / PR 引用等）的一页；
// since 非零时只返回此后更新的记录。
func (c *Client) ListTimeline(ctx context.Context, owner, repo string, index int64, since time.Time, o ListOptions) ([]*TimelineEvent, *Response, error) {
	q := url.Values{}
	o.apply(q)
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var ev []*TimelineEvent
	resp, err := c.do(ctx, "GET", issuePath(owner, repo, index)+"/timeline", q, nil, &ev)
	return ev, resp, err
}
