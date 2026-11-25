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
//
// 代次（gen）只在状态切换时递增：正常 → 暂停、试探失败 → 再暂停、试探成功 → 恢复正常。调用开始时取得 ticket，
// 结束时只有同一代的结果才计入：暂停前就已在途的调用（包括超时后仍未退出的）晚到的成功或失败不会关闭、重开熔断，
// 也不会干扰试探的结果。正常状态下的普通成功只清零连续失败计数、不换代，同代在途调用之后的失败照常计入。
type breaker struct {
	mu        sync.Mutex
	gen       uint64
	fails     int
	first     time.Time
	openUntil time.Time // 非零：已暂停过；已过期且未恢复时处于试探阶段
	probing   bool
}

// ticket 是一次调用在熔断中的凭据。
type ticket struct {
	gen   uint64
	probe bool // 暂停结束后的试探调用
}

// allow 报告能否调用。
func (b *breaker) allow(now time.Time) (ticket, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return ticket{gen: b.gen}, true
	}
	if now.Before(b.openUntil) || b.probing {
		return ticket{}, false
	}
	b.probing = true
	return ticket{gen: b.gen, probe: true}, true
}

func (b *breaker) success(t ticket) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.gen != b.gen {
		return // 上一代的调用：不计入
	}
	if b.openUntil.IsZero() {
		b.fails = 0 // 正常状态：清零连续失败，不换代
		return
	}
	if !t.probe {
		return // 试探阶段只认试探调用的结果
	}
	b.fails, b.openUntil, b.probing = 0, time.Time{}, false
	b.gen++ // 试探成功恢复正常：换代，暂停期间遗留的调用不再计入
}

// failure 记一次失败，返回是否因此暂停。
func (b *breaker) failure(t ticket, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.gen != b.gen || (!b.openUntil.IsZero() && !t.probe) {
		return false
	}
	if t.probe {
		b.open(now)
		return true
	}
	if b.fails == 0 || now.Sub(b.first) > failWindow {
		b.fails, b.first = 0, now
	}
	b.fails++
	if b.fails >= failThreshold {
		b.open(now)
		return true
	}
	return false
}

func (b *breaker) open(now time.Time) {
	b.probing, b.fails, b.openUntil = false, 0, now.Add(openFor)
	b.gen++
}

// neutral 结束一次既不算成功也不算失败的调用（拒绝、取消）：试探名额交还，下次调用重新试探。
func (b *breaker) neutral(t ticket) {
	if !t.probe {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.gen == b.gen {
		b.probing = false
	}
}
