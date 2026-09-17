package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// goroutineByState tracks the number of goroutines by state (sampled periodically).
// Note: total goroutine count is already registered by the Prometheus GoCollector, no need to duplicate.
var goroutineByState = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "rtc_goroutines_by_state",
		Help: "Number of goroutines by state (running, runnable, syscall, waiting, etc).",
	},
	[]string{"state"},
)

// StartGoroutineCollector starts a background goroutine metrics collector.
// It samples goroutine state distribution every 10 seconds and writes Prometheus metrics.
// The total goroutine count is provided automatically by the Prometheus GoCollector (go_goroutines metric);
// this collector only gathers the per-state breakdown.
// When the goroutine count exceeds leakThreshold, a warning log is emitted (threshold=0 disables the alert).
// Returns a cancel function to stop collecting (typically called in Server.Stop).
func StartGoroutineCollector(leakThreshold int) (cancel func()) {
	ticker := time.NewTicker(10 * time.Second)
	done := make(chan struct{})

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error(context.Background(), "StartGoroutineCollector panic recovered",
					zap.Any("panic", r),
					zap.String("stack", string(debug.Stack())),
				)
			}
		}()
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

// collectGoroutineMetrics collects a single snapshot of goroutine metrics.
func collectGoroutineMetrics(leakThreshold int) {
	n := runtime.NumGoroutine()

	// Count goroutine distribution by state.
	stacks := collectStacks()
	stateCounts := countByState(stacks)
	// Reset before setting to avoid stale data.
	goroutineByState.Reset()
	for state, count := range stateCounts {
		goroutineByState.WithLabelValues(state).Set(float64(count))
	}

	// Leak alert.
	if leakThreshold > 0 && n > leakThreshold {
		logger.Warn(context.Background(), "goroutine leak threshold exceeded",
			zap.Int("current", n),
			zap.Int("threshold", leakThreshold),
		)
	}
}

// stackEntry represents a stack trace summary for a single goroutine.
type stackEntry struct {
	ID    int    `json:"id"`
	State string `json:"state"`
	TopFn string `json:"top_function"`
	Stack string `json:"stack,omitempty"` // Only populated in detail mode.
}

// GoroutinesHandler returns the /debug/goroutines HTTP handler.
//
// Query parameters:
//   - detail=1: return the full stack trace for each goroutine (JSON array)
//   - default: return a summary (total count, state distribution, top function per goroutine)
//
// Response format: application/json
func GoroutinesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		detail := r.URL.Query().Get("detail") == "1"
		n := runtime.NumGoroutine()

		if !detail {
			// Summary mode.
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

		// Detail mode: return all goroutine stacks.
		buf := make([]byte, 1<<20) // 1MB initial buffer
		nbytes := runtime.Stack(buf, true)
		for nbytes == len(buf) {
			// Buffer too small, double and retry.
			buf = make([]byte, len(buf)*2)
			nbytes = runtime.Stack(buf, true)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write(buf[:nbytes]); err != nil {
			logger.Error(context.Background(), "write goroutine stacks", zap.Error(err))
		}
	}
}

// collectStacks parses runtime.Stack output, extracting each goroutine's ID and state.
func collectStacks() []stackEntry {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return parseStackOutput(string(buf[:n]))
}

// parseStackOutput parses goroutine stack trace text into structured data.
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

// parseHeaderLine parses a "goroutine N [state]:" formatted line.
func parseHeaderLine(line string) (id int, state, topFn string) {
	// Format: goroutine 123 [running]:
	//         func.name(args)
	var n int
	rest := line[len("goroutine "):]
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	id, _ = strconv.Atoi(rest[:n])

	// Extract state.
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

// countByState counts the number of goroutines in each state.
func countByState(entries []stackEntry) map[string]int {
	counts := make(map[string]int, len(entries))
	for _, e := range entries {
		counts[e.State]++
	}
	return counts
}
