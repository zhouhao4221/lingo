package gitea

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhouhao4221/lingo/internal/cache"
)

// TemplateLister 读取仓库 Issue 模板，由 Client 实现。
type TemplateLister interface {
	ListIssueTemplates(ctx context.Context, owner, repo string) ([]*IssueTemplate, error)
}

// TemplateCache 按仓库缓存 Issue 模板（默认 5 分钟，读取失败缓存 30 秒），供建单与 GET /ticket-types 共用，
// 避免每次都让 Gitea 重新解析全部模板文件。零值可用，可并发使用。
type TemplateCache struct {
	TTL time.Duration // 0 表示 5 分钟

	once sync.Once
	c    *cache.Cache[string, cachedTemplates]
	gen  atomic.Uint64 // Reset 时递增：进行中的读取结果属于旧连接，不写入缓存
	mu   sync.Mutex    // 代次检查与写入、Reset 互斥
}

type cachedTemplates struct {
	ts  []*IssueTemplate
	err error
}

// failureTTL 是读取失败的缓存时长。
const failureTTL = 30 * time.Second

func (c *TemplateCache) cache() *cache.Cache[string, cachedTemplates] {
	c.once.Do(func() {
		ttl := c.TTL
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		c.c = cache.New[string, cachedTemplates](ttl, 500)
	})
	return c.c
}

// Get 返回缓存中的模板，过期或没有时经 g 读取。并发未命中时可能各自读取一次。
func (c *TemplateCache) Get(ctx context.Context, g TemplateLister, owner, name string) ([]*IssueTemplate, error) {
	key := strings.ToLower(owner + "/" + name)
	if e, ok := c.cache().Get(key); ok {
		return e.ts, e.err
	}
	gen := c.gen.Load()
	ts, err := g.ListIssueTemplates(ctx, owner, name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen.Load() != gen {
		return ts, err // 期间连接已更换（Reset）：结果属于旧连接，不写入缓存
	}
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, err // 调用方取消，不是仓库的问题，不缓存
	case err != nil:
		c.cache().PutTTL(key, cachedTemplates{err: err}, failureTTL) // 仓库无权限等：短暂缓存，避免每次请求都打到 Gitea
	default:
		c.cache().Put(key, cachedTemplates{ts: ts})
	}
	return ts, err
}

// Reset 丢弃全部缓存并作废进行中的读取（Gitea 连接更换后调用）。
func (c *TemplateCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen.Add(1)
	c.cache().DeleteFunc(func(string) bool { return true })
}

// Forget 丢弃仓库的缓存。
func (c *TemplateCache) Forget(owner, name string) {
	c.cache().Delete(strings.ToLower(owner + "/" + name))
}
