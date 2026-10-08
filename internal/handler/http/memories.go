package httphandler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/memory"
)

// MemoriesHandler handles memory export HTTP requests.
type MemoriesHandler struct {
	memoryUC   *usecase.MemoryUsecase
	signer     *auth.JWTSigner
	banChecker middleware.UserBanChecker
}

// NewMemoriesHandler creates a MemoriesHandler.
func NewMemoriesHandler(memoryUC *usecase.MemoryUsecase, signer *auth.JWTSigner, banChecker middleware.UserBanChecker) *MemoriesHandler {
	return &MemoriesHandler{memoryUC: memoryUC, signer: signer, banChecker: banChecker}
}

// RegisterRoutes registers memory-related routes on the given ServeMux.
//
// The export endpoint requires JWT authentication. When allowDevBypass is true
// (development only), requests may use X-User-ID / X-Device-ID headers instead
// of a Bearer token.
func (h *MemoriesHandler) RegisterRoutes(mux *http.ServeMux, allowDevBypass bool) {
	authMiddleware := middleware.JWTAuth(h.signer, allowDevBypass, h.banChecker)
	mux.Handle("POST /api/memories/export", authMiddleware(http.HandlerFunc(h.ExportMemories)))
}

// ExportRequest defines the JSON request body for memory export.
type ExportRequest struct {
	Scope      string   `json:"scope"`       // "session" | "user" | "global"
	ScopeID    string   `json:"scope_id"`    // UUID string
	Format     string   `json:"format"`      // "okf-bundle"
	Types      []string `json:"types"`       // filter by types
	Tags       []string `json:"tags"`        // filter by tags
	IncludeLog bool     `json:"include_log"` // generate log.md
}

// ExportMemories handles POST /api/memories/export.
//
// It validates the request, creates an exporter, and streams a gzip-compressed
// OKF bundle to the response writer.
//
// Note: Since this uses streaming response (Content-Type: application/gzip),
// once writing to the response body begins, JSON error responses are no longer
// possible. Therefore:
//  1. All validation errors are checked before writing and return JSON errors
//  2. Errors during export are logged only, no error response is returned
//  3. Clients should check HTTP status code and Content-Length to determine success
func (h *MemoriesHandler) ExportMemories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Check if memory is configured
	if !h.memoryUC.IsMemoryConfigured() {
		httputil.WriteJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "memory.not_configured",
		})
		return
	}

	// Limit request body size to prevent abuse (1 MB should be more than enough)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Parse request body
	var req ExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"error": "memory.invalid_json",
		})
		return
	}

	// Validate scope
	if !memory.IsValidScopeType(memory.ScopeType(req.Scope)) {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"error": "memory.invalid_scope",
		})
		return
	}

	// Validate scopeId
	scopeID, err := uuid.Parse(req.ScopeID)
	if err != nil {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"error": "memory.invalid_scope_id",
		})
		return
	}

	// Validate format
	if req.Format != "okf-bundle" {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"error": "memory.unsupported_format",
		})
		return
	}

	// --- Authorization: verify scope ownership ---
	// JWTAuth middleware has already ensured userID is present in context.
	userID, _ := contextx.GetUserID(ctx)

	// Set response headers for gzip download
	filename := fmt.Sprintf("memories-%s-%s.tar.gz", req.Scope, time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	// Build export options
	opts := memory.ExportOptions{
		Scope:      memory.ScopeType(req.Scope),
		ScopeID:    scopeID,
		Types:      req.Types,
		Tags:       req.Tags,
		IncludeLog: req.IncludeLog,
	}

	logger.Info(ctx, "[memories.HTTP] export request",
		zap.String("scope", req.Scope),
		zap.String("scopeId", req.ScopeID),
		zap.String("format", req.Format))

	// Delegate to use case (handles ownership validation and export)
	if err := h.memoryUC.ExportMemories(ctx, userID, memory.ScopeType(req.Scope), scopeID, opts, w); err != nil {
		// Map use case errors to HTTP responses
		if errors.Is(err, usecase.ErrForbidden) {
			// Headers already set, but we can still write status
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
			return
		}
		if errors.Is(err, usecase.ErrSessionNotFound) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "session.not_found"})
			return
		}

		// Export failed after headers were sent
		logger.Error(ctx, "[memories.HTTP] export failed",
			zap.String("scope", req.Scope),
			zap.String("scopeId", req.ScopeID),
			zap.Error(err))
		// Headers may already be sent; log but don't try to write error response
		return
	}
}
