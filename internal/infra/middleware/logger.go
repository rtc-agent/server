package middleware

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// routeParamPattern matches dynamic path segments (UUIDs, numeric IDs, hex strings)
// to normalize span names and avoid high cardinality in traces.
// Examples: /api/v1/users/123 -> /api/v1/users/{param}
//
//	/api/v1/sessions/550e8400-e29b-41d4-a716-446655440000 -> /api/v1/sessions/{param}
var routeParamPattern = regexp.MustCompile(`/[0-9a-fA-F-]{8,}`)

// RequestLogger middleware creates a trace span for each request,
// records the trace_id in logs, and logs request duration and status code.
// For error responses (status >= 400), captures and logs the response body.
// Note: WebSocket upgrade requests skip tracing to avoid long-lived spans.
// Note: /healthz and /metrics paths are not logged to avoid noise.
func RequestLogger(next http.Handler) http.Handler {
	tracer := otel.Tracer("http")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip logging for /healthz and /metrics paths.
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		// Detect WebSocket upgrade requests.
		isWebSocketUpgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")

		// Skip tracing for WebSocket upgrade requests to avoid long-lived spans.
		// WebSocket connections can last for minutes/hours, creating very deep trace trees.
		if isWebSocketUpgrade {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()

		// SECURITY: Use r.URL.Path instead of r.URL.String() to avoid logging query parameters.
		// Query parameters may contain sensitive information like tokens, codes, or session IDs.
		// Only log the path portion to prevent accidental data leakage.
		//
		// Normalize route path to avoid high cardinality span names.
		// Since this is an HTTP middleware (not Gin), we don't have access to route templates.
		// Replace UUIDs, numeric IDs, and other dynamic path segments with {param}.
		// Special case: S3 routes use {bucket}/{key} pattern for clarity.
		spanName := r.Method + " " + normalizeRoutePath(r.URL.Path)

		// Create trace span.
		ctx, span := tracer.Start(r.Context(), spanName,
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.path", r.URL.Path),
				attribute.String("http.user_agent", r.UserAgent()),
			),
		)
		defer span.End()

		// Use httpsnoop to wrap the ResponseWriter, preserving Hijacker/Flusher/Pusher
		// interfaces while capturing status code and error response body.
		var (
			statusCode  int
			body        bytes.Buffer
			captureBody bool
			mu          sync.Mutex
		)
		captureBody = true

		wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					mu.Lock()
					statusCode = code
					mu.Unlock()
					next(code)
				}
			},
			Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(b []byte) (int, error) {
					mu.Lock()
					sc := statusCode
					mu.Unlock()
					// If status hasn't been set yet (implicit 200), sc will be 0.
					// Capture body for error responses (status >= 400).
					if captureBody && sc >= 400 {
						mu.Lock()
						body.Write(b)
						mu.Unlock()
					}
					return next(b)
				}
			},
		})

		next.ServeHTTP(wrapped, r.WithContext(ctx))

		duration := time.Since(start)

		// Default to 200 if WriteHeader was never called.
		mu.Lock()
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		finalStatus := statusCode
		errorBody := body.String()
		mu.Unlock()

		// Record span status.
		span.SetAttributes(attribute.Int("http.status_code", finalStatus))
		if finalStatus >= 500 {
			span.SetStatus(codes.Error, "server error")
		}

		// Build log fields.
		fields := []zap.Field{
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", finalStatus),
			zap.Duration("duration", duration),
			zap.String("trace_id", span.SpanContext().TraceID().String()),
		}

		// If this is an error response with a captured body, add error info.
		// SECURITY: Error responses may contain stack traces, SQL queries, or internal paths.
		// Only log the response body in debug mode to prevent sensitive data leakage.
		// In production, only the body length is recorded for observability without risk.
		if finalStatus >= 400 && len(errorBody) > 0 {
			if logger.IsDebugMode() {
				// In debug mode, log truncated body content for development troubleshooting.
				// 500 characters is sufficient for most error messages.
				if len(errorBody) > 500 {
					errorBody = errorBody[:500] + "...(truncated)"
				}
				fields = append(fields, zap.String("error_response", errorBody))
			} else {
				// In production, only log the body length to avoid sensitive data leakage.
				fields = append(fields, zap.Int("error_response_length", len(errorBody)))
			}
		}

		// Choose log level based on status code.
		if finalStatus >= 500 {
			logger.Error(ctx, "HTTP request failed", fields...)
		} else if finalStatus >= 400 {
			logger.Warn(ctx, "HTTP request client error", fields...)
		} else {
			logger.Info(ctx, "HTTP request", fields...)
		}
	})
}

// normalizeRoutePath replaces dynamic path segments (UUIDs, numeric IDs, hex strings)
// with {param} to produce low-cardinality span names for tracing.
// Special cases: /s3 routes use {bucket}/{key} for clarity.
func normalizeRoutePath(path string) string {
	// Special handling for S3 routes: /s3/{bucket}/{key}
	if strings.HasPrefix(path, "/s3/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/s3/"), "/", 2)
		if len(parts) == 1 {
			return "/s3/{bucket}"
		}
		return "/s3/{bucket}/{key}"
	}

	// Replace path segments that look like dynamic parameters:
	// - UUIDs (8+ hex chars with optional dashes)
	// - Numeric IDs
	// - Long hex strings
	// This keeps span names low-cardinality for tracing systems.
	return routeParamPattern.ReplaceAllString(path, "/{param}")
}
