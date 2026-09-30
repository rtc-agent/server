package httphandler

import (
	"errors"
	"net/http"

	"github.com/rtc-agent/server/internal/usecase"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
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
	// Skip signature verification for OPTIONS requests (CORS preflight)
	if r.Method == http.MethodOptions {
		m.next.ServeHTTP(w, r)
		return
	}

	// Extract AccessKeyID from Authorization header
	auth := r.Header.Get("Authorization")
	if auth == "" {
		WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Parse credential to get AccessKeyID
	cred, _, _, err := rtcoss3.ParseAuthorizationHeader(auth)
	if err != nil {
		WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
	}

	// Lookup credential from cache or DB
	credValue, err := m.oss3UC.LookupCredential(r.Context(), cred.AccessKeyID)
	if err != nil || credValue == nil {
		WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		return
	}

	// Verify signature
	if err := rtcoss3.VerifySigV4Request(r, credValue.SecretAccessKey, m.region, "s3"); err != nil {
		var s3err *rtcoss3.S3Error
		if errors.As(err, &s3err) {
			WriteS3Error(w, s3err, r.URL.Path, "")
		} else {
			WriteS3Error(w, rtcoss3.ErrInvalidSignature, r.URL.Path, "")
		}
		return
	}

	// Signature valid, call next handler
	m.next.ServeHTTP(w, r)
}
