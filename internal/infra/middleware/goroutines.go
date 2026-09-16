package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// goroutineByState 按状态分类的 goroutine 数量（定时采样）。
// 注意：go_goroutines 总数已由 Prometheus GoCollector 自动注册，无需重复。
var goroutineByState = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "rtc_goroutines_by_state",
		Help: "Number of goroutines by state (running, runnable, syscall, waiting, etc).",
	},
	[]string{"state"},
)

// StartGoroutineCollector 启动后台 goroutine 指标采集。
// 每 10 秒采样一次 goroutine 状态分布，写入 Prometheus 指标。
// goroutine 总数由 Prometheus GoCollector 自动提供（go_goroutines 指标），
// 此处仅额外采集按状态分类的细分指标。
// 当 goroutine 数量超过 leakThreshold 时记录告警日志（threshold=0 禁用告警）。
// 返回 cancel 函数，用于停止采集（通常在 Server.Stop 中调用）。
func StartGoroutineCollector(leakThreshold int) (cancel func()) {
	ticker := time.NewTicker(10 * time.Second)
	done := make(chan struct{})

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				collectGoroutineMetrics(leakThreshold)
			case <-done:
				return
			}
		}
	}()

	return func() { close(done) }
}

// collectGoroutineMetrics 采集一次 goroutine 指标。
func collectGoroutineMetrics(leakThreshold int) {
	n := runtime.NumGoroutine()

	// 按状态统计 goroutine 分布
	stacks := collectStacks()
	stateCounts := countByState(stacks)
	// 先重置再设置，避免 stale 数据
	goroutineByState.Reset()
	for state, count := range stateCounts {
		goroutineByState.WithLabelValues(state).Set(float64(count))
	}

	// 泄漏告警
	if leakThreshold > 0 && n > leakThreshold {
		logger.Warn(context.Background(), "goroutine leak threshold exceeded",
			zap.Int("current", n),
			zap.Int("threshold", leakThreshold),
		)
	}
}

// stackEntry 表示一个 goroutine 的堆栈摘要。
type stackEntry struct {
	ID    int    `json:"id"`
	State string `json:"state"`
	TopFn string `json:"top_function"`
	Stack string `json:"stack,omitempty"` // 仅在详细模式下填充
}

// GoroutinesHandler 返回 /debug/goroutines HTTP 处理器。
//
// 查询参数：
//   - detail=1：返回每个 goroutine 的完整堆栈（JSON 数组）
//   - 默认：返回摘要信息（总数、状态分布、各 goroutine 的栈顶函数）
//
// 响应格式：application/json
func GoroutinesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		detail := r.URL.Query().Get("detail") == "1"
		n := runtime.NumGoroutine()

		if !detail {
			// 摘要模式
			stacks := collectStacks()
			stateCounts := countByState(stacks)
			resp := struct {
				GoroutineCount int            `json:"goroutine_count"`
				ByState        map[string]int `json:"by_state"`
				Timestamp      string         `json:"timestamp"`
			}{
				GoroutineCount: n,
				ByState:        stateCounts,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				logger.Error(context.Background(), "encode goroutine summary", zap.Error(err))
			}
			return
		}

		// 详细模式：返回所有 goroutine 堆栈
		buf := make([]byte, 1<<20) // 1MB 初始缓冲区
		nbytes := runtime.Stack(buf, true)
		for nbytes == len(buf) {
			// 缓冲区不够，翻倍重试
			buf = make([]byte, len(buf)*2)
			nbytes = runtime.Stack(buf, true)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write(buf[:nbytes]); err != nil {
			logger.Error(context.Background(), "write goroutine stacks", zap.Error(err))
		}
	}
}

// collectStacks 解析 runtime.Stack 输出，提取每个 goroutine 的 ID 和状态。
func collectStacks() []stackEntry {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return parseStackOutput(string(buf[:n]))
}

// parseStackOutput 解析 goroutine 堆栈文本为结构化数据。
func parseStackOutput(output string) []stackEntry {
	var entries []stackEntry
	var current *stackEntry
	var stackLines []string

	flush := func() {
		if current != nil {
			if len(stackLines) > 0 {
				current.Stack = stackLines[0]
			}
			entries = append(entries, *current)
			current = nil
			stackLines = nil
		}
	}

	lines := splitLines(output)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if len(line) == 0 {
			flush()
			continue
		}
		if line[0] == '\n' || line[0] == '\r' {
			continue
		}
		if len(line) > 4 && line[:4] == "goro" {
			flush()
			id, state, topFn := parseHeaderLine(line)
			current = &stackEntry{ID: id, State: state, TopFn: topFn}
			continue
		}
		if current != nil {
			stackLines = append(stackLines, line)
		}
	}
	flush()
	return entries
}

// parseHeaderLine 解析 "goroutine N [state]:" 格式的行。
func parseHeaderLine(line string) (id int, state, topFn string) {
	// 格式: goroutine 123 [running]:
	//        func.name(args)
	var n int
	rest := line[len("goroutine "):]
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	id, _ = strconv.Atoi(rest[:n])

	// 提取状态
	if i := indexByte(rest[n:], '['); i >= 0 {
		if j := indexByte(rest[n+i:], ']'); j >= 0 {
			state = rest[n+i+1 : n+i+j]
		}
	}
	return
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// countByState 统计各状态的 goroutine 数量。
func countByState(entries []stackEntry) map[string]int {
	counts := make(map[string]int, len(entries))
	for _, e := range entries {
		counts[e.State]++
	}
	return counts
}
