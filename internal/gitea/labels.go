package gitea

import (
	"context"
	"net/url"
	"strconv"
)

// CreateLabelOption 是创建标签的参数；Color 为不带 # 的 6 位十六进制。
type CreateLabelOption struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description,omitempty"`
	Exclusive   bool   `json:"exclusive,omitempty"`
}

// ListLabels 返回仓库标签的一页。
func (c *Client) ListLabels(ctx context.Context, owner, repo string, o ListOptions) ([]*Label, *Response, error) {
	q := url.Values{}
	o.apply(q)
	var ls []*Label
	resp, err := c.do(ctx, "GET", repoPath(owner, repo)+"/labels", q, nil, &ls)
	return ls, resp, err
}

// ListOrgLabels 返回组织标签的一页；组织标签对组织下所有仓库可用。
func (c *Client) ListOrgLabels(ctx context.Context, org string, o ListOptions) ([]*Label, *Response, error) {
	q := url.Values{}
	o.apply(q)
	var ls []*Label
	resp, err := c.do(ctx, "GET", "/orgs/"+esc(org)+"/labels", q, nil, &ls)
	return ls, resp, err
}

// CreateLabel 在仓库中创建标签。
func (c *Client) CreateLabel(ctx context.Context, owner, repo string, opt CreateLabelOption) (*Label, error) {
	var l Label
	_, err := c.do(ctx, "POST", repoPath(owner, repo)+"/labels", nil, opt, &l)
	return &l, err
}

// AddIssueLabels 给 Issue 追加标签（标签 id），返回 Issue 当前的全部标签。
func (c *Client) AddIssueLabels(ctx context.Context, owner, repo string, index int64, labelIDs []int64) ([]*Label, error) {
	var ls []*Label
	_, err := c.do(ctx, "POST", issuePath(owner, repo, index)+"/labels", nil, map[string][]int64{"labels": labelIDs}, &ls)
	return ls, err
}

// ReplaceIssueLabels 用 labelIDs 替换 Issue 的全部标签。
func (c *Client) ReplaceIssueLabels(ctx context.Context, owner, repo string, index int64, labelIDs []int64) ([]*Label, error) {
	var ls []*Label
	_, err := c.do(ctx, "PUT", issuePath(owner, repo, index)+"/labels", nil, map[string][]int64{"labels": labelIDs}, &ls)
	return ls, err
}

// RemoveIssueLabel 移除 Issue 上的一个标签。
func (c *Client) RemoveIssueLabel(ctx context.Context, owner, repo string, index, labelID int64) error {
	_, err := c.do(ctx, "DELETE", issuePath(owner, repo, index)+"/labels/"+strconv.FormatInt(labelID, 10), nil, nil, nil)
	return err
}
