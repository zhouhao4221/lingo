// Package cache 是进程内的键值缓存：每项有过期时间，总数有上限（超出时淘汰最早写入的），可并发使用。
// 灵问的状态都在数据库，这里只缓存可随时重新读取的数据（Issue 模板、标签 ID、建单整理结果），多实例各自缓存。
package cache

import (
	"sync"
	"time"
)

// Cache 是带过期时间与容量上限的缓存。零值不可用，用 New 创建。
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	now   func() time.Time
	items map[K]entry[V]
	order []K // 写入顺序，用于容量淘汰；过期与删除的键惰性清理
}

type entry[V any] struct {
	v   V
	exp time.Time
}

// New 创建缓存：ttl 为默认过期时间（≤0 表示不过期），max 为最多条目数（≤0 表示 1000）。
func New[K comparable, V any](ttl time.Duration, max int) *Cache[K, V] {
	if max <= 0 {
		max = 1000
	}
	return &Cache[K, V]{ttl: ttl, max: max, now: time.Now, items: map[K]entry[V]{}}
}

// Get 返回未过期的值。
func (c *Cache[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[k]
	if !ok || (!e.exp.IsZero() && !c.now().Before(e.exp)) {
		if ok {
			delete(c.items, k)
		}
		var zero V
		return zero, false
	}
	return e.v, true
}

// Put 以默认过期时间写入。
func (c *Cache[K, V]) Put(k K, v V) { c.PutTTL(k, v, c.ttl) }

// PutTTL 以 ttl 写入（≤0 表示不过期），超出容量时淘汰最早写入的条目。
func (c *Cache[K, V]) PutTTL(k K, v V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	if _, ok := c.items[k]; !ok {
		c.order = append(c.order, k)
	}
	c.items[k] = entry[V]{v, exp}
	for len(c.items) > c.max && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.items, old)
	}
	if len(c.order) > 2*c.max { // 清掉已删除键留下的顺序记录
		keep := c.order[:0]
		seen := make(map[K]bool, len(c.items))
		for _, k := range c.order {
			if _, ok := c.items[k]; ok && !seen[k] {
				seen[k] = true
				keep = append(keep, k)
			}
		}
		c.order = keep
	}
}

// Delete 删除一项。
func (c *Cache[K, V]) Delete(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, k)
}

// DeleteFunc 删除 match 返回真的项。
func (c *Cache[K, V]) DeleteFunc(match func(K) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.items {
		if match(k) {
			delete(c.items, k)
		}
	}
}

// Len 返回当前条目数（含尚未清理的过期项）。
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// SetClock 替换时钟（测试用）。
func (c *Cache[K, V]) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}
