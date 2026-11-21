// Package cache 是进程内的键值缓存：每项有过期时间，总数有上限（超出时淘汰最早写入的），可并发使用。
// 灵工的状态都在数据库，这里只缓存可随时重新读取的数据（Issue 模板、标签 ID、建单整理结果），多实例各自缓存。
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
	seq   uint64
	items map[K]entry[V]
	order []slot[K] // 写入顺序；条目被覆盖、删除或过期后，旧的顺序记录凭 seq 识别为失效并跳过
}

type entry[V any] struct {
	v   V
	exp time.Time
	seq uint64
}

type slot[K comparable] struct {
	k   K
	seq uint64
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

// PutTTL 以 ttl 写入（≤0 表示不过期）；每次写入都算最新，超出容量时淘汰写入最早的条目。
func (c *Cache[K, V]) PutTTL(k K, v V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	c.seq++
	c.items[k] = entry[V]{v, exp, c.seq}
	c.order = append(c.order, slot[K]{k, c.seq})
	for len(c.items) > c.max && len(c.order) > 0 {
		s := c.order[0]
		c.order = c.order[1:]
		if e, ok := c.items[s.k]; ok && e.seq == s.seq {
			delete(c.items, s.k)
		}
	}
	if len(c.order) > 2*c.max { // 去掉失效的顺序记录
		keep := c.order[:0]
		for _, s := range c.order {
			if e, ok := c.items[s.k]; ok && e.seq == s.seq {
				keep = append(keep, s)
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
