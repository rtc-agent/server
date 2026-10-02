package httphandler

import (
	"net/http"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.uber.org/zap"
)

// SigV4Middleware wraps an HTTP handler with SigV4 signature verification.
type SigV4Middleware struct {
	oss3UC *usecase.OSS3Usecase
	region string
	next   http.Handler
}

// NewSigV4Middleware creates a new SigV4 middleware.
func NewSigV4Middleware(oss3UC *usecase.OSS3Usecase, region string, next http.Handler) *SigV4Middleware {
	return &SigV4Middleware{
		oss3UC: oss3UC,
		region: region,
		next:   next,
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
	auth := r.Header.Get("Authorization")
	if auth == "" {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Parse credential to get AccessKeyID
	cred, _, _, err := rtcoss3.ParseAuthorizationHeader(auth)
	if err != nil {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
	}

	// Lookup credential from cache or DB
	credValue, err := m.oss3UC.LookupCredential(r.Context(), cred.AccessKeyID)
	if err != nil {
		// Actual DB/cache error — return 500 (not 403) so the client can retry
		logger.Error(r.Context(), "SigV4: credential lookup failed",
			zap.String("access_key_id", cred.AccessKeyID),
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
		logger.Error(r.Context(), "SigV4: signature verification failed",
			zap.String("access_key_id", cred.AccessKeyID),
			zap.Error(err))
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
	}

	// Decrypt SessionToken to extract user_id
	payload, err := m.oss3UC.DecryptSessionToken(credValue.SessionToken)
	if err != nil {
		logger.Error(r.Context(), "SigV4: session token decryption failed",
			zap.String("access_key_id", cred.AccessKeyID),
			zap.Error(err))
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
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
