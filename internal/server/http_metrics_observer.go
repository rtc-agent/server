// http_metrics_observer.go adapts LLMResponseObserver to the turn-agent Metrics interface.
// It extracts model name from the HTTP request and records LLM call metrics.

package server

import (
	"context"
	"net/http"
	"time"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// httpMetricsObserver implements LLMResponseObserver and records metrics
// via the turn-agent Metrics interface.
type httpMetricsObserver struct {
	metrics turnagent.Metrics
}

// NewHTTPMetricsObserver creates an observer that records LLM metrics.
func NewHTTPMetricsObserver(metrics turnagent.Metrics) LLMResponseObserver {
	return &httpMetricsObserver{metrics: metrics}
}

// Observe implements LLMResponseObserver.
func (o *httpMetricsObserver) Observe(req *http.Request, model string, statusCode int, elapsed time.Duration, usage TokenUsage, stream bool) {
	// Record HTTP-level metrics
	o.metrics.RecordLLMHTTPRequest(context.Background(), turnagent.LLMHTTPMetricsAttrs{
		Model:        model,
		StatusCode:   statusCode,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		DurationMs:   elapsed.Milliseconds(),
	})
}
