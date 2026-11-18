package gitea

import (
	"context"
	"net/url"
	"strconv"
)

// ListReleasesOptions 是列出 Release 的条件。
type ListReleasesOptions struct {
	ListOptions
	IncludeDrafts bool  // 默认不含草稿
	Prerelease    *bool // nil 不过滤
}

// ListReleases 返回仓库 Release 的一页（按创建时间倒序）。
func (c *Client) ListReleases(ctx context.Context, owner, repo string, o ListReleasesOptions) ([]*Release, *Response, error) {
	q := url.Values{}
	o.ListOptions.apply(q)
	if !o.IncludeDrafts {
		q.Set("draft", "false")
	}
	if o.Prerelease != nil {
		q.Set("pre-release", strconv.FormatBool(*o.Prerelease))
	}
	var rs []*Release
	resp, err := c.do(ctx, "GET", repoPath(owner, repo)+"/releases", q, nil, &rs)
	return rs, resp, err
}

// GetReleaseByTag 返回 tag 对应的 Release。
func (c *Client) GetReleaseByTag(ctx context.Context, owner, repo, tag string) (*Release, error) {
	var r Release
	_, err := c.do(ctx, "GET", repoPath(owner, repo)+"/releases/tags/"+esc(tag), nil, nil, &r)
	return &r, err
}

// ListTags 返回仓库标签（Git tag）的一页，用于只打 tag 不建 Release 的仓库。
func (c *Client) ListTags(ctx context.Context, owner, repo string, o ListOptions) ([]*Tag, *Response, error) {
	q := url.Values{}
	o.apply(q)
	var ts []*Tag
	resp, err := c.do(ctx, "GET", repoPath(owner, repo)+"/tags", q, nil, &ts)
	return ts, resp, err
}

// Compare 返回 base...head 之间的提交，用于找出两个版本 tag 之间包含的修复。
func (c *Client) Compare(ctx context.Context, owner, repo, base, head string) (*Compare, error) {
	var cp Compare
	_, err := c.do(ctx, "GET", repoPath(owner, repo)+"/compare/"+esc(base)+"..."+esc(head), nil, nil, &cp)
	return &cp, err
}

// GetPullRequest 返回 PR。
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, index int64) (*PullRequest, error) {
	var pr PullRequest
	_, err := c.do(ctx, "GET", repoPath(owner, repo)+"/pulls/"+strconv.FormatInt(index, 10), nil, nil, &pr)
	return &pr, err
}

// GetCommitPullRequest 返回合并了提交 sha 的 PR；提交不是经 PR 合入时 errors.Is(err, ErrNotFound)。
func (c *Client) GetCommitPullRequest(ctx context.Context, owner, repo, sha string) (*PullRequest, error) {
	var pr PullRequest
	_, err := c.do(ctx, "GET", repoPath(owner, repo)+"/commits/"+esc(sha)+"/pull", nil, nil, &pr)
	return &pr, err
}

// ListCommitStatuses 返回提交（或分支、tag）上的状态，用于识别 deploy/production 等上线信号。
func (c *Client) ListCommitStatuses(ctx context.Context, owner, repo, ref string, o ListOptions) ([]*CommitStatus, *Response, error) {
	q := url.Values{}
	o.apply(q)
	var ss []*CommitStatus
	resp, err := c.do(ctx, "GET", repoPath(owner, repo)+"/commits/"+esc(ref)+"/statuses", q, nil, &ss)
	return ss, resp, err
}
