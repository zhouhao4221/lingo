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
	at  time.Time
	ts  []*IssueTemplate
	err error // 读取失败也短暂缓存，避免仓库无权限等情况下每次请求都打到 Gitea
}

// failureTTL 是读取失败的缓存时长。
const failureTTL = 30 * time.Second

// Get 返回缓存中的模板，过期或没有时经 g 读取；读取失败缓存 30 秒。并发未命中时可能各自读取一次。
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
	if e, ok := c.byRepo[key]; ok {
		if age := now().Sub(e.at); (e.err == nil && age < ttl) || (e.err != nil && age < failureTTL) {
			c.mu.Unlock()
			return e.ts, e.err
		}
	}
	c.mu.Unlock()
	ts, err := g.ListIssueTemplates(ctx, owner, name)
	if err != nil && ctx.Err() != nil {
		return nil, err // 调用方取消，不是仓库的问题，不缓存
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byRepo == nil {
		c.byRepo = map[string]cachedTemplates{}
	}
	c.byRepo[key] = cachedTemplates{at: now(), ts: ts, err: err}
	return ts, err
}

// Forget 丢弃仓库的缓存。
func (c *TemplateCache) Forget(owner, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byRepo, strings.ToLower(owner+"/"+name))
}
