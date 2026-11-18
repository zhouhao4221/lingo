package gitea

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

func commentPath(owner, repo string, id int64) string {
	return repoPath(owner, repo) + "/issues/comments/" + strconv.FormatInt(id, 10)
}

// CreateComment 在 Issue 下发表评论。
func (c *Client) CreateComment(ctx context.Context, owner, repo string, index int64, body string) (*Comment, error) {
	var cm Comment
	_, err := c.do(ctx, "POST", issuePath(owner, repo, index)+"/comments", nil, map[string]string{"body": body}, &cm)
	return &cm, err
}

// EditComment 修改评论正文。
func (c *Client) EditComment(ctx context.Context, owner, repo string, id int64, body string) (*Comment, error) {
	var cm Comment
	_, err := c.do(ctx, "PATCH", commentPath(owner, repo, id), nil, map[string]string{"body": body}, &cm)
	return &cm, err
}

// GetComment 返回单条评论。
func (c *Client) GetComment(ctx context.Context, owner, repo string, id int64) (*Comment, error) {
	var cm Comment
	_, err := c.do(ctx, "GET", commentPath(owner, repo, id), nil, nil, &cm)
	return &cm, err
}

// ListIssueComments 返回 Issue 的全部评论（该接口不分页）；since 非零时只返回此后更新的。
func (c *Client) ListIssueComments(ctx context.Context, owner, repo string, index int64, since time.Time) ([]*Comment, error) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var cs []*Comment
	_, err := c.do(ctx, "GET", issuePath(owner, repo, index)+"/comments", q, nil, &cs)
	return cs, err
}

// ListRepoComments 返回仓库内所有 Issue 评论的一页，用于对账增量拉取；
// 评论所属 Issue 见 Comment.IssueURL。
func (c *Client) ListRepoComments(ctx context.Context, owner, repo string, since time.Time, o ListOptions) ([]*Comment, *Response, error) {
	q := url.Values{}
	o.apply(q)
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var cs []*Comment
	resp, err := c.do(ctx, "GET", repoPath(owner, repo)+"/issues/comments", q, nil, &cs)
	return cs, resp, err
}

// AddCommentReaction 给评论加表情回应，content 如 "eyes"、"+1"、"-1"、"heart"。
func (c *Client) AddCommentReaction(ctx context.Context, owner, repo string, id int64, content string) (*Reaction, error) {
	var r Reaction
	_, err := c.do(ctx, "POST", commentPath(owner, repo, id)+"/reactions", nil, map[string]string{"content": content}, &r)
	return &r, err
}

// IssueIndexFromURL 从评论的 IssueURL（如 .../issues/12）中取出 Issue 编号，失败返回 0。
func IssueIndexFromURL(issueURL string) int64 {
	u, err := url.Parse(issueURL)
	if err != nil {
		return 0
	}
	p := u.Path
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			n, err := strconv.ParseInt(p[i+1:], 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}
