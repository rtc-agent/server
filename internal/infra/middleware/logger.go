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

// responseWriter 包装 http.ResponseWriter 以捕获状态码和错误响应体
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
	// 如果是错误响应且需要捕获body，则复制到buffer
	if rw.captureBody && rw.statusCode >= 400 {
		rw.body.Write(b)
	}
	return rw.ResponseWriter.Write(b)
}

// RequestLogger 请求日志中间件
// 为每个请求创建 trace span，记录 trace_id 到日志，并记录请求耗时和状态码。
// 对于错误响应（状态码 >= 400），会捕获并记录响应体中的错误信息。
// 注意：WebSocket 升级请求不会被包装，以避免干扰协议升级。
func RequestLogger(next http.Handler) http.Handler {
	tracer := otel.Tracer("http")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 检测是否是 WebSocket 升级请求
		isWebSocketUpgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")

		start := time.Now()

		// 创建 trace span
		ctx, span := tracer.Start(r.Context(), r.Method+" "+r.URL.Path,
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.path", r.URL.Path),
				attribute.String("http.user_agent", r.UserAgent()),
			),
		)
		defer span.End()

		// 对于 WebSocket 升级请求，不包装 ResponseWriter，直接传递原始的
		// 这样可以避免干扰 WebSocket 协议升级
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

		// 普通 HTTP 请求：包装 ResponseWriter 以捕获状态码和错误响应体
		rw := &responseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
			body:           &bytes.Buffer{},
			captureBody:    true,
		}

		next.ServeHTTP(rw, r.WithContext(ctx))

		duration := time.Since(start)

		// 记录 span 状态
		span.SetAttributes(attribute.Int("http.status_code", rw.statusCode))
		if rw.statusCode >= 500 {
			span.SetStatus(codes.Error, "server error")
		}

		// 构建日志字段
		fields := []zap.Field{
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rw.statusCode),
			zap.Duration("duration", duration),
			zap.String("trace_id", span.SpanContext().TraceID().String()),
		}

		// 如果是错误响应且捕获到了body，添加错误信息
		if rw.statusCode >= 400 && rw.body.Len() > 0 {
			errorBody := rw.body.String()
			// 限制错误信息长度，避免日志过大
			if len(errorBody) > 500 {
				errorBody = errorBody[:500] + "...(truncated)"
			}
			fields = append(fields, zap.String("error_response", errorBody))
		}

		// 根据状态码选择日志级别
		if rw.statusCode >= 500 {
			logger.Error(ctx, "HTTP request failed", fields...)
		} else if rw.statusCode >= 400 {
			logger.Warn(ctx, "HTTP request client error", fields...)
		} else {
			logger.Info(ctx, "HTTP request", fields...)
		}
	})
}
