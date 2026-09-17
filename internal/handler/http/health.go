// Package httphandler provides the protocol adaptation layer for HTTP APIs.
//
// It includes health check (healthz/readyz), OAuth2 endpoints, Interrupt
// submission, and other HTTP routes. All HTTP responses are formatted
// uniformly via the httputil package.
package httphandler

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/svc"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// readyzLimiter rate-limits the /readyz endpoint to prevent DoS attacks.
// 10 requests per second with a burst of 10.
var readyzLimiter = rate.NewLimiter(rate.Every(time.Second/10), 10)

// Handler is the HTTP request handler.
type Handler struct {
	svcCtx *svc.ServiceContext
}

// NewHandler creates a new HTTP handler.
func NewHandler(svcCtx *svc.ServiceContext) *Handler {
	return &Handler{svcCtx: svcCtx}
}

// Healthz is the liveness probe endpoint.
// It returns whether the service is running without checking dependencies.
// Used for K8s livenessProbe.
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz is the readiness probe endpoint.
// It checks whether dependencies (DB, Redis, Centrifuge) are available.
// Used for K8s readinessProbe. Includes rate limiting to prevent
// overloading downstream dependencies with frequent checks.
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	// Rate limiting: reject excessive requests to prevent DoS
	if !readyzLimiter.Allow() {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := make(map[string]string)
	allReady := true

	// Check database connectivity.
	if err := h.svcCtx.DB.WithContext(ctx).Exec("SELECT 1").Error; err != nil {
		checks["db"] = "error"
		allReady = false
		logger.Warn(ctx, "Readyz: DB check failed", zap.Error(err))
	} else {
		checks["db"] = "ok"
	}

	// Check Redis connectivity.
	if err := h.svcCtx.Redis.Ping(ctx).Err(); err != nil {
		checks["redis"] = "error"
		allReady = false
		logger.Warn(ctx, "Readyz: Redis check failed", zap.Error(err))
	} else {
		checks["redis"] = "ok"
	}

	// Check Centrifuge node status.
	if h.svcCtx.CentrifugeNode == nil {
		checks["centrifuge"] = "not configured"
		allReady = false
	} else {
		// Centrifuge node has no direct Ping method; check if the node is
		// running via Info(). If the node has shut down, Info() returns an error.
		if _, err := h.svcCtx.CentrifugeNode.Info(); err != nil {
			checks["centrifuge"] = "error"
			allReady = false
			logger.Warn(ctx, "Readyz: Centrifuge check failed", zap.Error(err))
		} else {
			checks["centrifuge"] = "ok"
		}
	}

	status := http.StatusOK
	response := map[string]any{"status": "ready", "checks": checks}

	if !allReady {
		status = http.StatusServiceUnavailable
		response["status"] = "not ready"
		logger.Warn(ctx, "Readyz check failed", zap.Any("checks", checks))
	}

	httputil.WriteJSON(w, status, response)
}
