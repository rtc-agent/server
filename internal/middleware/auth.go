package middleware

import (
	"net/http"
	"strings"

	"github.com/rtc-agent/server/internal/auth"
	"github.com/rtc-agent/server/internal/contextx"

	"github.com/google/uuid"
)

// JWTAuth JWT 鉴权中间件构造函数。
// signer 为 JWT 签名器；allowDevBypass 仅在开发环境设为 true，
// 允许通过 X-User-ID / X-Device-ID header 绕过 JWT 验证（生产环境必须为 false）。
func JWTAuth(signer *auth.JWTSigner, allowDevBypass bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var userID uuid.UUID
			var deviceID string

			// 1. 尝试从 Authorization header 解析 JWT
			if token := extractBearerToken(r); token != "" {
				claims, err := signer.ParseAccessToken(token)
				if err != nil {
					http.Error(w, "unauthorized: invalid token", http.StatusUnauthorized)
					return
				}
				userID = claims.UserID
				deviceID = claims.DeviceID
			} else if allowDevBypass {
				// 2. 开发回退：从 header 直接读取（必须显式启用）
				uidStr := r.Header.Get("X-User-ID")
				did := r.Header.Get("X-Device-ID")
				if uidStr == "" || did == "" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				parsed, err := uuid.Parse(uidStr)
				if err != nil {
					http.Error(w, "unauthorized: invalid user id", http.StatusUnauthorized)
					return
				}
				userID = parsed
				deviceID = did
			} else {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			ctx := r.Context()
			ctx = contextx.WithClientInfo(ctx, userID, deviceID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearerToken 从 Authorization header 提取 token
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}
