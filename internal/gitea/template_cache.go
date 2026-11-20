package gitea

import (
	"context"
	"strings"
	"sync"
	"time"
)

// TemplateLister 读取仓库 Issue 模板，由 Client 实现。
type TemplateLister interface {
	ListIssueTemplates(ctx context.Context, owner, repo string) ([]*IssueTemplate, error)
}

// TemplateCache 按仓库缓存 Issue 模板（默认 5 分钟），供建单与 GET /ticket-types 共用，避免每次都让 Gitea
// 重新解析全部模板文件。零值可用，可并发使用。
type TemplateCache struct {
	TTL time.Duration // 0 表示 5 分钟
	Now func() time.Time

	mu     sync.Mutex
	byRepo map[string]cachedTemplates
}

type cachedTemplates struct {
	at time.Time
	ts []*IssueTemplate
}

// Get 返回缓存中的模板，过期或没有时经 g 读取；读取失败不缓存。
func (c *TemplateCache) Get(ctx context.Context, g TemplateLister, owner, name string) ([]*IssueTemplate, error) {
	key := strings.ToLower(owner + "/" + name)
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	c.mu.Lock()
	if e, ok := c.byRepo[key]; ok && now().Sub(e.at) < ttl {
		c.mu.Unlock()
		return e.ts, nil
	}
	c.mu.Unlock()
	ts, err := g.ListIssueTemplates(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byRepo == nil {
		c.byRepo = map[string]cachedTemplates{}
	}
	c.byRepo[key] = cachedTemplates{at: now(), ts: ts}
	return ts, nil
}

// Forget 丢弃仓库的缓存。
func (c *TemplateCache) Forget(owner, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byRepo, strings.ToLower(owner+"/"+name))
}
