package auth

import (
	"sync"
	"time"
)

// Limiter 是一个按 key 计数的登录失败限速器（仅内存，不落库）。
//
// 设计取舍：
//   - 不持久化：重启管理平台即清空计数是可接受的（重启本身需要机器权限），
//     换来的是零磁盘写入与更简单的一致性问题。
//   - 计数窗口与锁定时间分开：窗口内累计失败达到上限才锁定，
//     这样偶发的手误不会让管理员被锁在门外。
//
// 对应 01-prd F02「连续失败有限速」。
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	lock   time.Duration
	items  map[string]*limiterItem
}

type limiterItem struct {
	fails       int
	windowStart time.Time
	lockedUntil time.Time
}

// NewLimiter 构造限速器。
//
// max <= 0 时取 5 次；window 默认 10 分钟；lock 默认 15 分钟。
func NewLimiter(max int, window, lock time.Duration) *Limiter {
	if max <= 0 {
		max = 5
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	if lock <= 0 {
		lock = 15 * time.Minute
	}
	return &Limiter{
		max:    max,
		window: window,
		lock:   lock,
		items:  make(map[string]*limiterItem),
	}
}

// Allow 判断该 key 当前是否允许尝试登录。
//
// 第二个返回值是剩余锁定时间，仅在被拒绝时非零；限速器把锁定状态暴露出来，
// 是为了让界面能给出「请 N 分钟后再试」，而不是含糊的「登录失败」。
func (l *Limiter) Allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	it, ok := l.items[key]
	if !ok {
		return true, 0
	}
	if now.Before(it.lockedUntil) {
		return false, it.lockedUntil.Sub(now)
	}
	// 锁定已过：清掉旧计数，重新开始。
	if !it.lockedUntil.IsZero() {
		delete(l.items, key)
		return true, 0
	}
	// 计费窗口已过：重置窗口。
	if now.Sub(it.windowStart) > l.window {
		delete(l.items, key)
	}
	return true, 0
}

// Fail 记录一次失败尝试，达到上限则进入锁定。
func (l *Limiter) Fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	it, ok := l.items[key]
	if !ok || now.Sub(it.windowStart) > l.window {
		l.items[key] = &limiterItem{fails: 1, windowStart: now}
		return
	}
	it.fails++
	if it.fails >= l.max {
		it.lockedUntil = now.Add(l.lock)
	}
}

// Reset 在登录成功后清除该 key 的失败计数。
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.items, key)
}

// Cleanup 清理已过期的条目，避免 map 随攻击流量无限增长。
func (l *Limiter) Cleanup(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for k, it := range l.items {
		expired := now.After(it.lockedUntil) && now.Sub(it.windowStart) > l.window
		if expired {
			delete(l.items, k)
		}
	}
}

// MaxFailures 返回触发锁定的失败次数，供界面提示。
func (l *Limiter) MaxFailures() int { return l.max }
