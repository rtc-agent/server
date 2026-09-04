package middleware

import (
	"net/http"
	"time"

	"github.com/rtc-agent/server/pkg/logger"
)

// RequestLogger 请求日志中间件
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("HTTP %s %s %s",
			r.Method,
			r.URL.Path,
			time.Since(start),
		)
	})
}
