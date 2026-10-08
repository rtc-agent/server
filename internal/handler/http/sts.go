package httphandler

import (
	"encoding/json"
	"net/http"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// STSHandler handles Security Token Service (STS) API endpoints.
// These endpoints are authenticated via JWT and allow users to:
//   - Issue temporary S3 credentials (POST /api/credentials/temporary)
type STSHandler struct {
	oss3UC     *usecase.OSS3Usecase
	signer     *auth.JWTSigner
	isDev      bool
	banChecker middleware.UserBanChecker
}

// NewSTSHandler creates a new STS handler.
func NewSTSHandler(oss3UC *usecase.OSS3Usecase, signer *auth.JWTSigner, isDev bool, banChecker middleware.UserBanChecker) *STSHandler {
	return &STSHandler{
		oss3UC:     oss3UC,
		signer:     signer,
		isDev:      isDev,
		banChecker: banChecker,
	}
}

// RegisterRoutes registers STS API routes on the given mux.
func (h *STSHandler) RegisterRoutes(mux *http.ServeMux) {
	if h.oss3UC == nil {
		// OSS3 disabled — STS endpoints not registered.
		return
	}

	authMiddleware := middleware.JWTAuth(h.signer, h.isDev, h.banChecker)

	// POST /api/credentials/temporary — issue temporary S3 credentials
	mux.Handle("POST /api/credentials/temporary", authMiddleware(http.HandlerFunc(h.IssueTemporaryCredentials)))
}

// IssueTemporaryCredentials generates temporary S3 credentials for the authenticated user.
//
// Request: POST /api/credentials/temporary (JWT authenticated)
// Response: 200 OK with JSON body:
//
//	{
//	  "access_key_id": "AKIA...",
//	  "secret_access_key": "...",
//	  "session_token": "...",
//	  "expires_at": "2026-09-30T23:00:00Z"
//	}
func (h *STSHandler) IssueTemporaryCredentials(w http.ResponseWriter, r *http.Request) {
	userID, ok := contextx.GetUserID(r.Context())
	if !ok {
		httputil.WriteError(w, http.StatusUnauthorized, "credentials.unauthorized", "authentication required")
		return
	}

	result, err := h.oss3UC.IssueTemporaryCredentials(r.Context(), userID.String())
	if err != nil {
		logger.Error(r.Context(), "failed to issue temporary credentials",
			zap.String("user_id", userID.String()),
			zap.Error(err),
		)
		httputil.WriteError(w, http.StatusInternalServerError, "credentials.internal_error", "failed to issue credentials")
		return
	}

	resp := map[string]interface{}{
		"access_key_id":     result.AccessKeyID,
		"secret_access_key": result.SecretAccessKey,
		"session_token":     result.SessionToken,
		"expires_at":        result.ExpiresAt.Format("2006-01-02T15:04:05Z"),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warn(r.Context(), "failed to encode response", zap.Error(err))
	}
}
