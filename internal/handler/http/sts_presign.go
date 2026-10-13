package httphandler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.uber.org/zap"
)

// STSPresignHandler handles presigned URL generation.
//
// Endpoints:
//   - POST /api/presigned-url — generate a presigned PUT or GET URL
type STSPresignHandler struct {
	oss3UC      *usecase.OSS3Usecase
	signer      *auth.JWTSigner
	isDev       bool
	banChecker  middleware.UserBanChecker
	rateLimiter RateLimiterInterface
	serverEnv   string
}

// NewSTSPresignHandler creates a new presigned URL handler.
func NewSTSPresignHandler(oss3UC *usecase.OSS3Usecase, signer *auth.JWTSigner, isDev bool, banChecker middleware.UserBanChecker, rateLimiter RateLimiterInterface, serverEnv string) *STSPresignHandler {
	return &STSPresignHandler{
		oss3UC:      oss3UC,
		signer:      signer,
		isDev:       isDev,
		banChecker:  banChecker,
		rateLimiter: rateLimiter,
		serverEnv:   serverEnv,
	}
}

// RegisterRoutes registers presigned URL API routes on the given mux.
func (h *STSPresignHandler) RegisterRoutes(mux *http.ServeMux) {
	if h.oss3UC == nil {
		// OSS3 disabled — presign endpoints not registered.
		return
	}

	authMiddleware := middleware.JWTAuth(h.signer, h.isDev, h.banChecker, h.serverEnv)

	// POST /api/presigned-url — generate a presigned URL
	// Rate limiting is applied before authentication to prevent DoS attacks
	handler := authMiddleware(http.HandlerFunc(h.GeneratePresignedURL))
	if h.rateLimiter != nil {
		handler = STSRateLimitMiddleware(h.rateLimiter, handler)
	}
	mux.Handle("POST /api/presigned-url", handler)
}

// presignRequest is the JSON body for presigned URL generation.
type presignRequest struct {
	Operation   string `json:"operation"`    // "put" or "get"
	Key         string `json:"key"`          // e.g. "user-123/file-id"
	ExpiresIn   int64  `json:"expires_in"`   // seconds; default 900 (15min); max 604800 (7 days)
	ContentType string `json:"content_type"` // required for "put"; enforced via POST policy
}

// presignResponse is the JSON response for presigned URL generation.
type presignResponse struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"` // "PUT" or "POST"
	ExpiresAt string            `json:"expires_at"`
	Fields    map[string]string `json:"fields,omitempty"` // POST form fields (for POST method)
}

// GeneratePresignedURL generates a presigned URL for upload or download.
//
// Request: POST /api/presigned-url (JWT authenticated)
// Body:
//
//	{
//	  "operation": "put" | "get",
//	  "key": "user-123/file-id",
//	  "expires_in": 900,
//	  "content_type": "image/png"   // required for "put"
//	}
//
// Response: 200 OK with JSON body:
//
//	{
//	  "url": "https://s3.rtc-agent.local/rtc-agent",
//	  "method": "POST",
//	  "expires_at": "2026-09-30T23:00:00Z",
//	  "fields": {"key": "user-123/file-id", "Content-Type": "image/png", ...}
//	}
func (h *STSPresignHandler) GeneratePresignedURL(w http.ResponseWriter, r *http.Request) {
	userID, ok := contextx.GetUserID(r.Context())
	if !ok {
		httputil.WriteError(r.Context(), w, http.StatusUnauthorized, "presign.unauthorized", "authentication required")
		return
	}

	// Limit request body size to 1MB
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req presignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writePresignError(w, r, http.StatusBadRequest, "presign.invalid_request", "invalid request body")
		return
	}

	// Validate operation
	if req.Operation != "put" && req.Operation != "get" {
		writePresignError(w, r, http.StatusBadRequest, "presign.invalid_request", "operation must be 'put' or 'get'")
		return
	}

	// Validate key
	if req.Key == "" {
		writePresignError(w, r, http.StatusBadRequest, "presign.invalid_request", "key is required")
		return
	}

	// Validate key format (user-{uuid}/{md5}.{ext})
	// Fail early before generating a presigned URL that would fail on use.
	if s3Err := rtcoss3.ValidateKey(req.Key, userID.String()); s3Err != nil {
		writePresignError(w, r, s3Err.HTTPCode, "presign.invalid_key", s3Err.Message)
		return
	}

	// Default expiry: 15 minutes, max: 7 days (AWS S3 limit).
	expiresIn := time.Duration(req.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 15 * time.Minute
	}
	const maxPresignExpiry = 7 * 24 * time.Hour
	if expiresIn > maxPresignExpiry {
		writePresignError(w, r, http.StatusBadRequest, "presign.invalid_request", "expires_in must not exceed 7 days (604800 seconds)")
		return
	}

	// Validate Content-Type for PUT operations
	if req.Operation == "put" {
		if req.ContentType == "" {
			writePresignError(w, r, http.StatusBadRequest, "presign.invalid_request", "content_type is required for put operation")
			return
		}
		if !rtcoss3.IsAllowedContentType(req.ContentType) {
			writePresignError(w, r, http.StatusBadRequest, "presign.invalid_content_type", "content_type is not allowed. Only image/* and text/* types are permitted")
			return
		}
	}

	// P1-5 fix: TODO - Presigned GET URLs can be used unlimited times within their validity period.
	// Consider adding download count tracking for sensitive or high-value files:
	// - Option 1: Single-use GET URLs (invalidate after first use via Redis flag)
	// - Option 2: Download count limit (e.g., max 100 downloads per URL)
	// - Option 3: Time-based rate limiting (e.g., max 10 downloads per hour)
	// This is not critical for now because:
	// 1. Presigned URLs expire (default 15min, max 7 days)
	// 2. Files are content-addressed (MD5 in key), so unauthorized sharing is less risky
	// 3. The user's own quota is not affected by downloads (only uploads)
	// However, for compliance or DRM requirements, download tracking may be needed.
	url, expiresAt, fields, err := h.oss3UC.GeneratePresignedURL(r.Context(), userID.String(), req.Operation, req.Key, expiresIn, req.ContentType)
	if err != nil {
		// Check if it's a permission/validation error
		if isPermissionError(err) {
			writePresignError(w, r, http.StatusForbidden, "presign.forbidden", "Access denied")
			return
		}
		if errors.Is(err, usecase.ErrMissingContentType) || errors.Is(err, usecase.ErrUnsupportedContentType) {
			// Use a fixed user-friendly message instead of exposing err.Error(),
			// which may leak internal implementation details (e.g. allowed MIME types list,
			// S3 policy structure). Pre-validation in GeneratePresignedURL catches most
			// cases; this branch handles race conditions with policy updates.
			writePresignError(w, r, http.StatusBadRequest, "presign.invalid_content_type", "invalid or unsupported content_type")
			return
		}
		logger.Error(r.Context(), "failed to generate presigned URL",
			zap.String("user_id", userID.String()),
			zap.String("operation", req.Operation),
			zap.String("key", req.Key),
			zap.Error(err),
		)
		writePresignError(w, r, http.StatusInternalServerError, "presign.internal_error", "failed to generate presigned URL")
		return
	}

	method := "GET"
	if req.Operation == "put" {
		method = "POST"
	}

	resp := presignResponse{
		URL:       url,
		Method:    method,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		Fields:    fields,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warn(r.Context(), "failed to encode presign response", zap.Error(err))
	}
}

// isPermissionError checks if the error is a permission/validation error.
func isPermissionError(err error) bool {
	return errors.Is(err, usecase.ErrKeyPrefixViolation) ||
		errors.Is(err, usecase.ErrUnsupportedPresignOperation) ||
		errors.Is(err, usecase.ErrExpiryExceeded)
}

// writePresignError writes a JSON error response with consistent format.
func writePresignError(w http.ResponseWriter, r *http.Request, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	resp := map[string]string{
		"error":             code,
		"error_description": description,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warn(r.Context(), "failed to encode error response", zap.Error(err))
	}
}
