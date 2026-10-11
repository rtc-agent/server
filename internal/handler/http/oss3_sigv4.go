package httphandler

import (
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// maskAccessKeyID masks an access key ID for safe logging.
// Shows only the first 8 characters followed by "..." to prevent credential leakage.
// If the key is shorter than 8 characters, returns "..." to avoid revealing the full value.
func maskAccessKeyID(key string) string {
	if len(key) < 8 {
		return "..."
	}
	return key[:8] + "..."
}

// SigV4Middleware wraps an HTTP handler with SigV4 signature verification.
type SigV4Middleware struct {
	oss3UC     *usecase.OSS3Usecase
	region     string
	banChecker middleware.UserBanChecker
	next       http.Handler
}

// NewSigV4Middleware creates a new SigV4 middleware.
// banChecker optionally checks if a user is banned (can be nil to skip check).
func NewSigV4Middleware(oss3UC *usecase.OSS3Usecase, region string, banChecker middleware.UserBanChecker, next http.Handler) *SigV4Middleware {
	return &SigV4Middleware{
		oss3UC:     oss3UC,
		region:     region,
		banChecker: banChecker,
		next:       next,
	}
}

// ServeHTTP implements the http.Handler interface.
func (m *SigV4Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger.Debug(r.Context(), "SigV4 middleware: request received",
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path))

	// Skip signature verification for OPTIONS requests (CORS preflight)
	if r.Method == http.MethodOptions {
		m.next.ServeHTTP(w, r)
		return
	}

	// Extract AccessKeyID from Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Parse credential to get AccessKeyID
	cred, _, _, err := rtcoss3.ParseAuthorizationHeader(authHeader)
	if err != nil {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
	}

	// Validate AccessKeyID length to prevent cache key exhaustion attacks.
	// AWS AccessKeyID is always exactly 20 characters (uppercase alphanumeric).
	if len(cred.AccessKeyID) != 20 {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
	}

	// Lookup credential from cache or DB
	credValue, err := m.oss3UC.LookupCredential(r.Context(), cred.AccessKeyID)
	if err != nil {
		// Actual DB/cache error — return 500 (not 403) so the client can retry
		// Mask access key ID in logs (show only first 8 chars) to prevent credential leakage
		maskedKey := maskAccessKeyID(cred.AccessKeyID)
		logger.Error(r.Context(), "SigV4: credential lookup failed",
			zap.String("access_key_id", maskedKey),
			zap.Error(err))
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}
	if credValue == nil {
		// Credential not found — return 403
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
	}

	// Verify signature
	if err := rtcoss3.VerifySigV4Request(r, credValue.SecretAccessKey, m.region, "s3"); err != nil {
		// Mask access key ID in logs (show only first 8 chars)
		maskedKey := maskAccessKeyID(cred.AccessKeyID)
		logger.Error(r.Context(), "SigV4: signature verification failed",
			zap.String("access_key_id", maskedKey),
			zap.Error(err))
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
	}

	// Decrypt SessionToken to extract user_id
	payload, err := m.oss3UC.DecryptSessionToken(credValue.SessionToken)
	if err != nil {
		// Mask access key ID in logs (show only first 8 chars)
		maskedKey := maskAccessKeyID(cred.AccessKeyID)
		logger.Error(r.Context(), "SigV4: session token decryption failed",
			zap.String("access_key_id", maskedKey),
			zap.Error(err))
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
	}

	// SECURITY (P1 fix): Check if user is banned before allowing access.
	// Even if OSS3 credentials were revoked via BanUser, cached credentials
	// could still be used within their TTL. This check closes that window
	// by verifying ban status on every authenticated request.
	//
	// Follows the same fail-open strategy as JWT middleware for resilience:
	// - If Redis cache is unavailable, falls back to DB check
	// - If both cache and DB are unavailable, request is allowed (fail-open)
	// See JWTAuth in middleware/auth.go for detailed rationale.
	if m.banChecker != nil {
		parsedUserID, parseErr := uuid.Parse(payload.UserID)
		if parseErr != nil {
			maskedKey := maskAccessKeyID(cred.AccessKeyID)
			logger.Error(r.Context(), "SigV4: invalid user_id in session token",
				zap.String("access_key_id", maskedKey),
				zap.String("user_id", payload.UserID),
				zap.Error(parseErr))
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
			return
		}

		banned, cached := middleware.GetBanCache(r.Context(), parsedUserID)
		if !cached {
			var banErr error
			banned, banErr = m.banChecker.IsUserBanned(r.Context(), parsedUserID)
			if banErr != nil {
				// Fail-open: log the error but allow the request to proceed
				logger.Warn(r.Context(), "SigV4: failed to check user ban status - fail-open applied",
					zap.Error(banErr),
					zap.String("user_id", parsedUserID.String()),
					zap.String("remote_addr", r.RemoteAddr),
					zap.Bool("bypass_applied", true))
				auth.RecordSecurityEvent("ban_check_fail_open")
			} else {
				// Successfully checked ban status, cache it
				middleware.SetBanCache(r.Context(), parsedUserID, banned)
			}
		}

		if banned {
			logger.Info(r.Context(), "SigV4: banned user rejected",
				zap.String("user_id", parsedUserID.String()),
				zap.String("remote_addr", r.RemoteAddr))
			rtcoss3.WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
			return
		}
	}

	// Inject user_id into context for downstream handlers.
	// NOTE: RequestID is intentionally NOT set here — RequestIDMiddleware already
	// handles it (generating a UUID if the client didn't provide one). Overwriting
	// it here with r.Header.Get("X-Amz-Request-Id") would replace the UUID with
	// an empty string when the client omits the header.
	ctx := contextx.WithOSS3UserID(r.Context(), payload.UserID)

	// Set response header so AccessLogMiddleware can read the authenticated user ID
	// without needing a complex context capture pattern.
	w.Header().Set(responseHeaderUserKey, payload.UserID)

	// Signature valid, call next handler with enriched context
	m.next.ServeHTTP(w, r.WithContext(ctx))
}
