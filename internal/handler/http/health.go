// Package httphandler provides the protocol adaptation layer for HTTP APIs.
//
// It includes health check (healthz/readyz), OAuth2 endpoints, Interrupt
// submission, and other HTTP routes. All HTTP responses are formatted
// uniformly via the httputil package.
package httphandler

import (
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// readyzLimiter rate-limits the /readyz endpoint to prevent DoS attacks.
// 10 requests per second with a burst of 10.
var readyzLimiter = rate.NewLimiter(rate.Every(time.Second/10), 10)

// Handler is the HTTP request handler.
//
// NOTE: Handler holds only the usecase-level HealthChecker — it intentionally
// avoids any direct dependency on *gorm.DB / redis.UniversalClient / *centrifuge.Node
// so that the layering rule (Handler -> Usecase -> Repo) is preserved.
// Infrastructure probes are delegated to usecase.HealthChecker.
type Handler struct {
	healthChecker *usecase.HealthChecker
}

// NewHandler creates a new HTTP handler.
func NewHandler(healthChecker *usecase.HealthChecker) *Handler {
	return &Handler{healthChecker: healthChecker}
}

// Healthz is the liveness probe endpoint.
// It returns whether the service is running without checking dependencies.
// Used for K8s livenessProbe.
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(r.Context(), w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz is the readiness probe endpoint.
// It delegates dependency checks to the usecase.HealthChecker, which pings
// DB/Redis/Centrifuge and returns an aggregated status.
// Includes rate limiting to prevent overloading downstream dependencies with
// frequent checks.
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	// Rate limiting: reject excessive requests to prevent DoS
	if !readyzLimiter.Allow() {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	health := h.healthChecker.Readiness(r.Context())

	status := http.StatusOK
	if !health.AllReady {
		status = http.StatusServiceUnavailable
		logger.Warn(r.Context(), "Readyz check failed", zap.Any("checks", health.Components))
	}

	response := map[string]any{
		"status": map[bool]string{true: "ready", false: "not ready"}[health.AllReady],
		"checks": health.Components,
	}

	httputil.WriteJSON(r.Context(), w, status, response)
}
