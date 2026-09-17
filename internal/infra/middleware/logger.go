package middleware

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// responseWriter wraps http.ResponseWriter to capture status codes and error response bodies.
type responseWriter struct {
	http.ResponseWriter
	statusCode  int
	body        *bytes.Buffer
	captureBody bool
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	// If this is an error response and body capture is enabled, copy to buffer.
	if rw.captureBody && rw.statusCode >= 400 {
		rw.body.Write(b)
	}
	return rw.ResponseWriter.Write(b)
}

// RequestLogger middleware creates a trace span for each request,
// records the trace_id in logs, and logs request duration and status code.
// For error responses (status >= 400), captures and logs the response body.
// Note: WebSocket upgrade requests are not wrapped to avoid interfering with protocol upgrade.
// Note: /healthz and /metrics paths are not logged to avoid noise.
func RequestLogger(next http.Handler) http.Handler {
	tracer := otel.Tracer("http")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip logging for /healthz and /metrics paths.
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		// Detect WebSocket upgrade requests.
		isWebSocketUpgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")

		start := time.Now()

		// Create trace span.
		ctx, span := tracer.Start(r.Context(), r.Method+" "+r.URL.Path,
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.path", r.URL.Path),
				attribute.String("http.user_agent", r.UserAgent()),
			),
		)
		defer span.End()

		// For WebSocket upgrade requests, pass the raw ResponseWriter without wrapping
		// to avoid interfering with the WebSocket protocol upgrade.
		if isWebSocketUpgrade {
			next.ServeHTTP(w, r.WithContext(ctx))

			duration := time.Since(start)
			span.SetAttributes(attribute.String("http.type", "websocket_upgrade"))

			logger.Info(ctx, "WebSocket upgrade request completed",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Duration("duration", duration),
				zap.String("trace_id", span.SpanContext().TraceID().String()),
			)
			return
		}

		// Regular HTTP requests: wrap ResponseWriter to capture status code and error body.
		rw := &responseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
			body:           &bytes.Buffer{},
			captureBody:    true,
		}

		next.ServeHTTP(rw, r.WithContext(ctx))

		duration := time.Since(start)

		// Record span status.
		span.SetAttributes(attribute.Int("http.status_code", rw.statusCode))
		if rw.statusCode >= 500 {
			span.SetStatus(codes.Error, "server error")
		}

		// Build log fields.
		fields := []zap.Field{
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rw.statusCode),
			zap.Duration("duration", duration),
			zap.String("trace_id", span.SpanContext().TraceID().String()),
		}

		// If this is an error response with a captured body, add error info.
		if rw.statusCode >= 400 && rw.body.Len() > 0 {
			errorBody := rw.body.String()
			// Limit error body length to avoid oversized logs.
			if len(errorBody) > 500 {
				errorBody = errorBody[:500] + "...(truncated)"
			}
			fields = append(fields, zap.String("error_response", errorBody))
		}

		// Choose log level based on status code.
		if rw.statusCode >= 500 {
			logger.Error(ctx, "HTTP request failed", fields...)
		} else if rw.statusCode >= 400 {
			logger.Warn(ctx, "HTTP request client error", fields...)
		} else {
			logger.Info(ctx, "HTTP request", fields...)
		}
	})
}
