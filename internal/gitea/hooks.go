package gitea

import (
	"context"
	"net/url"
	"strconv"
)

// LingoHookEvents 是灵问需要订阅的 Webhook 事件。
var LingoHookEvents = []string{
	"issues", "issue_comment", "issue_assign", "issue_label", "issue_milestone",
	"pull_request", "release", "create", "status", "workflow_run",
}

// CreateHookOption 是创建 Webhook 的参数。
type CreateHookOption struct {
	Type                string            `json:"type"` // 灵问使用 "gitea"
	Config              map[string]string `json:"config"`
	Events              []string          `json:"events"`
	Active              bool              `json:"active"`
	BranchFilter        string            `json:"branch_filter,omitempty"`
	AuthorizationHeader string            `json:"authorization_header,omitempty"`
	Name                string            `json:"name,omitempty"`
}

// EditHookOption 是修改 Webhook 的参数。
type EditHookOption struct {
	Config              map[string]string `json:"config,omitempty"`
	Events              []string          `json:"events,omitempty"`
	Active              *bool             `json:"active,omitempty"`
	AuthorizationHeader *string           `json:"authorization_header,omitempty"`
}

// NewLingoHook 返回指向 target 的 gitea 类型 Webhook 配置（JSON 载荷、共享密钥签名）。
func NewLingoHook(target, secret, authorization string) CreateHookOption {
	return CreateHookOption{
		Type:                "gitea",
		Config:              map[string]string{"url": target, "content_type": "json", "secret": secret},
		Events:              LingoHookEvents,
		Active:              true,
		AuthorizationHeader: authorization,
		Name:                "lingo",
	}
}

func hookPath(owner, repo string, id int64) string {
	return repoPath(owner, repo) + "/hooks/" + strconv.FormatInt(id, 10)
}

// ListHooks 返回仓库 Webhook 的一页。
func (c *Client) ListHooks(ctx context.Context, owner, repo string, o ListOptions) ([]*Hook, *Response, error) {
	q := url.Values{}
	o.apply(q)
	var hs []*Hook
	resp, err := c.do(ctx, "GET", repoPath(owner, repo)+"/hooks", q, nil, &hs)
	return hs, resp, err
}

// CreateHook 创建仓库 Webhook（令牌需 write:repository）。
func (c *Client) CreateHook(ctx context.Context, owner, repo string, opt CreateHookOption) (*Hook, error) {
	var h Hook
	_, err := c.do(ctx, "POST", repoPath(owner, repo)+"/hooks", nil, opt, &h)
	return &h, err
}

// EditHook 修改仓库 Webhook。
func (c *Client) EditHook(ctx context.Context, owner, repo string, id int64, opt EditHookOption) (*Hook, error) {
	var h Hook
	_, err := c.do(ctx, "PATCH", hookPath(owner, repo, id), nil, opt, &h)
	return &h, err
}

// TestHook 让 Gitea 向该 Webhook 发送一次测试投递（push 事件），用于启动自检连通性；
// 投递结果需在接收端确认，本调用成功只表示 Gitea 已发起投递。
func (c *Client) TestHook(ctx context.Context, owner, repo string, id int64) error {
	_, err := c.do(ctx, "POST", hookPath(owner, repo, id)+"/tests", nil, nil, nil)
	return err
}

// FindHook 在仓库 Webhook 中查找投递地址为 target 的一个，没有时返回 nil。
func (c *Client) FindHook(ctx context.Context, owner, repo, target string) (*Hook, error) {
	hs, err := ListAll(ctx, 0, func(ctx context.Context, o ListOptions) ([]*Hook, *Response, error) {
		return c.ListHooks(ctx, owner, repo, o)
	})
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if h.URL() == target {
			return h, nil
		}
	}
	return nil, nil
}
