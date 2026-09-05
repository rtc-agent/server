package middleware

import (
	"net/http"
	"strings"
)

// CORS 跨域中间件
// allowedOrigins 为空且 isDevelopment 为 true 时回退到 "*"；
// 生产环境（isDevelopment=false）必须显式配置 allow_origins，否则不设置 CORS 头。
func CORS(allowedOrigins []string, isDevelopment bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			originAllowed := false

			if len(allowedOrigins) == 0 {
				if isDevelopment {
					// 仅开发环境回退：允许任意 origin
					originAllowed = true
					w.Header().Set("Access-Control-Allow-Origin", "*")
				}
				// 生产环境 + 空 allow_origins：不设置 CORS 头（拒绝跨域）
			} else if isOriginAllowed(origin, allowedOrigins) {
				originAllowed = true
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}

			// 仅在 origin 被允许时设置附加 CORS 头，避免向未授权来源泄露能力信息
			if originAllowed {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func isOriginAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, "*") || strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}
