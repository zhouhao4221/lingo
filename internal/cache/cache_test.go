package cache

import (
	"testing"
	"time"
)

func TestCache(t *testing.T) {
	now := time.Unix(0, 0)
	c := New[string, int](time.Minute, 2)
	c.SetClock(func() time.Time { return now })
	c.Put("a", 1)
	c.PutTTL("b", 2, 0) // 不过期
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatal("a missing")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.Get("a"); ok {
		t.Error("a not expired")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b expired")
	}
	c.Put("c", 3)
	c.Put("d", 4) // 超出容量：淘汰最早写入的 b
	if _, ok := c.Get("b"); ok || c.Len() != 2 {
		t.Errorf("eviction: len=%d", c.Len())
	}
	c.DeleteFunc(func(k string) bool { return k == "c" })
	if _, ok := c.Get("c"); ok {
		t.Error("c not deleted")
	}
	for i := range 100 { // 顺序记录不会无限增长
		c.Put("x", i)
		c.Delete("x")
	}
	if len(c.order) > 4 {
		t.Errorf("order grew to %d", len(c.order))
	}
}
