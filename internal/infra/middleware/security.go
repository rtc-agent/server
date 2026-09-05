package middleware

import "net/http"

// SecurityHeaders 安全响应头中间件。
// 为所有响应添加通用的安全头部，防止常见 Web 攻击。
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'")

		next.ServeHTTP(w, r)
	})
}
