package agent

import (
	"sync"
	"time"
)

// Throttle 节流器：确保同一 key 的操作间隔 >= minInterval。
//
// 在间隔内的后续调用会被延迟到间隔结束后执行（只保留最后一次）。
// 使用 time.AfterFunc 而非 goroutine + sleep，避免 goroutine 泄漏。
//
// 每个 key 独立节流，互不干扰。
type Throttle struct {
	minInterval time.Duration
	mu          sync.Mutex
	lastCall    map[string]time.Time
	pendingFn   map[string]func()
	timers      map[string]*time.Timer
}

// NewThrottle 创建节流器。
func NewThrottle(minInterval time.Duration) *Throttle {
	return &Throttle{
		minInterval: minInterval,
		lastCall:    make(map[string]time.Time),
		pendingFn:   make(map[string]func()),
		timers:      make(map[string]*time.Timer),
	}
}

// Do 执行函数 fn，受节流控制。
//
//   - 如果距上次执行 >= minInterval（或首次调用）：立即执行 fn
//   - 如果在间隔内：记录 fn 为待执行（覆盖之前的），等待定时器触发后执行最后一次
//
// 同一个 key 在间隔内多次调用 Do，只有最后一次的 fn 会被执行。
// 不同 key 互不影响。
func (t *Throttle) Do(key string, fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()

	if last, ok := t.lastCall[key]; ok && now.Sub(last) < t.minInterval {
		// 在节流间隔内，记录待执行函数（覆盖之前的），等待定时器触发
		t.pendingFn[key] = fn
		return
	}

	// 立即执行
	t.lastCall[key] = now
	fn()

	// 如果已有定时器在跑，不需要再创建（定时器到期时会检查 pendingFn）
	if _, exists := t.timers[key]; exists {
		return
	}

	// 创建定时器，间隔结束后检查并执行待执行的函数
	last := t.lastCall[key]
	t.timers[key] = time.AfterFunc(t.minInterval, func() {
		t.mu.Lock()
		delete(t.timers, key)
		// 将 lastCall 设为理论间隔结束点（而非 time.Now()），
		// 避免 timer 延迟触发导致后续调用的时间差计算偏小。
		t.lastCall[key] = last.Add(t.minInterval)
		if pendingFn, ok := t.pendingFn[key]; ok {
			delete(t.pendingFn, key)
			t.mu.Unlock()
			pendingFn()
		} else {
			t.mu.Unlock()
		}
	})
}
