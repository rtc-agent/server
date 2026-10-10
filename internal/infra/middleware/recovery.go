package middleware

import (
	"net/http"
	"runtime/debug"

	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// Recovery middleware recovers from panics and returns a 500 Internal Server Error.
// It logs the panic and stack trace for debugging.
//
// This middleware should be placed at the outermost layer of the middleware chain
// to catch panics from all inner middleware and handlers.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				// Log the panic with stack trace for debugging.
				stack := string(debug.Stack())
				logger.Error(r.Context(), "panic recovered",
					zap.Any("error", err),
					zap.String("stack", stack),
					zap.String("method", r.Method),
					zap.String("path", r.URL.Path),
				)

				// Return 500 Internal Server Error to the client.
				// Don't expose the panic details to avoid information leakage.
				http.Error(w,
					"Internal Server Error",
					http.StatusInternalServerError,
				)
			}
		}()

		next.ServeHTTP(w, r)
	})
}
