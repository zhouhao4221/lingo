package connector

import (
	"sync"
	"time"
)

// 熔断参数。
const (
	failThreshold = 5
	failWindow    = 5 * time.Minute
	openFor       = time.Minute
)

// breaker 是单个工具的熔断状态（只在当前实例内存中）：5 分钟内连续失败 5 次后暂停 1 分钟；暂停结束后放行一次试探，
// 成功恢复、失败再暂停 1 分钟。
type breaker struct {
	mu        sync.Mutex
	fails     int
	first     time.Time
	openUntil time.Time // 非零：已打开过；已过期且未恢复时处于试探阶段
	probing   bool
}

// allow 报告能否调用；probe 为真表示这是暂停结束后的试探调用。
func (b *breaker) allow(now time.Time) (ok, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true, false
	}
	if now.Before(b.openUntil) || b.probing {
		return false, false
	}
	b.probing = true
	return true, true
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails, b.openUntil, b.probing = 0, time.Time{}, false
}

// failure 记一次失败，返回是否因此暂停。
func (b *breaker) failure(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probing {
		b.probing, b.fails, b.openUntil = false, 0, now.Add(openFor)
		return true
	}
	if b.fails == 0 || now.Sub(b.first) > failWindow {
		b.fails, b.first = 0, now
	}
	b.fails++
	if b.fails >= failThreshold {
		b.fails, b.openUntil = 0, now.Add(openFor)
		return true
	}
	return false
}

// neutral 结束一次既不算成功也不算失败的调用（拒绝、取消）：试探名额交还，下次调用重新试探。
func (b *breaker) neutral(probe bool) {
	if !probe {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}
