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
	oss3UC     *usecase.OSS3Usecase
	signer     *auth.JWTSigner
	isDev      bool
	banChecker middleware.UserBanChecker
}

// NewSTSPresignHandler creates a new presigned URL handler.
func NewSTSPresignHandler(oss3UC *usecase.OSS3Usecase, signer *auth.JWTSigner, isDev bool, banChecker middleware.UserBanChecker) *STSPresignHandler {
	return &STSPresignHandler{
		oss3UC:     oss3UC,
		signer:     signer,
		isDev:      isDev,
		banChecker: banChecker,
	}
}

// RegisterRoutes registers presigned URL API routes on the given mux.
func (h *STSPresignHandler) RegisterRoutes(mux *http.ServeMux) {
	if h.oss3UC == nil {
		// OSS3 disabled — presign endpoints not registered.
		return
	}

	authMiddleware := middleware.JWTAuth(h.signer, h.isDev, h.banChecker)

	// POST /api/presigned-url — generate a presigned URL
	mux.Handle("POST /api/presigned-url", authMiddleware(http.HandlerFunc(h.GeneratePresignedURL)))
}

// presignRequest is the JSON body for presigned URL generation.
type presignRequest struct {
	Operation string `json:"operation"`  // "put" or "get"
	Key       string `json:"key"`        // e.g. "user-123/file-id"
	ExpiresIn int64  `json:"expires_in"` // seconds; default 3600; max 604800 (7 days)
}

// presignResponse is the JSON response for presigned URL generation.
type presignResponse struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// GeneratePresignedURL generates a presigned URL for upload or download.
//
// Request: POST /api/presigned-url (JWT authenticated)
// Body:
//
//	{
//	  "operation": "put" | "get",
//	  "key": "user-123/file-id",
//	  "expires_in": 3600
//	}
//
// Response: 200 OK with JSON body:
//
//	{
//	  "url": "https://s3.rtc-agent.local/rtc-agent/user-123/file-id?...",
//	  "expires_at": "2026-09-30T23:00:00Z"
//	}
func (h *STSPresignHandler) GeneratePresignedURL(w http.ResponseWriter, r *http.Request) {
	userID, ok := contextx.GetUserID(r.Context())
	if !ok {
		httputil.WriteError(w, http.StatusUnauthorized, "presign.unauthorized", "authentication required")
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

	// Default expiry: 1 hour, max: 7 days (AWS S3 limit).
	expiresIn := time.Duration(req.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = time.Hour
	}
	const maxPresignExpiry = 7 * 24 * time.Hour
	if expiresIn > maxPresignExpiry {
		writePresignError(w, r, http.StatusBadRequest, "presign.invalid_request", "expires_in must not exceed 7 days (604800 seconds)")
		return
	}

	url, expiresAt, err := h.oss3UC.GeneratePresignedURL(r.Context(), userID.String(), req.Operation, req.Key, expiresIn)
	if err != nil {
		// Check if it's a permission error
		if isPermissionError(err) {
			writePresignError(w, r, http.StatusForbidden, "presign.forbidden", err.Error())
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

	resp := presignResponse{
		URL:       url,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
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
