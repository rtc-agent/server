package middleware

import (
	"net/http"
	"strings"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/contextx"

	"github.com/google/uuid"
)

// JWTAuth creates a JWT authentication middleware.
// signer is the JWT signer; allowDevBypass should only be true in development,
// allowing X-User-ID / X-Device-ID headers to bypass JWT validation (must be false in production).
func JWTAuth(signer *auth.JWTSigner, allowDevBypass bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var userID uuid.UUID
			var deviceID string

			// 1. Try to parse JWT from Authorization header.
			if token := extractBearerToken(r); token != "" {
				claims, err := signer.ParseAccessToken(token)
				if err != nil {
					http.Error(w, "unauthorized: invalid token", http.StatusUnauthorized)
					return
				}
				userID = claims.UserID
				deviceID = claims.DeviceID
			} else if allowDevBypass {
				// 2. Dev fallback: read directly from headers (must be explicitly enabled).
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

// extractBearerToken extracts the token from the Authorization header.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}
