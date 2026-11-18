package gitea

import "context"

// GetCurrentUser 返回令牌对应的用户（机器人账号），用于识别 Webhook 中机器人自己触发的事件。
func (c *Client) GetCurrentUser(ctx context.Context) (*User, error) {
	var u User
	_, err := c.do(ctx, "GET", "/user", nil, nil, &u)
	return &u, err
}

// GetVersion 返回实例版本，如 "1.27.3"。
func (c *Client) GetVersion(ctx context.Context) (string, error) {
	var v ServerVersion
	_, err := c.do(ctx, "GET", "/version", nil, nil, &v)
	return v.Version, err
}

// GetAPISettings 返回实例 API 设置（分页上限等）。
func (c *Client) GetAPISettings(ctx context.Context) (*APISettings, error) {
	var s APISettings
	_, err := c.do(ctx, "GET", "/settings/api", nil, nil, &s)
	return &s, err
}

// GetAttachmentSettings 返回实例附件设置（是否启用、类型、大小与数量上限）。
func (c *Client) GetAttachmentSettings(ctx context.Context) (*AttachmentSettings, error) {
	var s AttachmentSettings
	_, err := c.do(ctx, "GET", "/settings/attachment", nil, nil, &s)
	return &s, err
}

// GetRepo 返回仓库信息。
func (c *Client) GetRepo(ctx context.Context, owner, repo string) (*Repository, error) {
	var r Repository
	_, err := c.do(ctx, "GET", repoPath(owner, repo), nil, nil, &r)
	return &r, err
}

// GetCollaboratorPermission 返回 user 对仓库的权限。
func (c *Client) GetCollaboratorPermission(ctx context.Context, owner, repo, user string) (*CollaboratorPermission, error) {
	var p CollaboratorPermission
	_, err := c.do(ctx, "GET", repoPath(owner, repo)+"/collaborators/"+esc(user)+"/permission", nil, nil, &p)
	return &p, err
}
