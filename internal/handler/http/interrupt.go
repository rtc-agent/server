package httphandler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// InterruptHandler handles interrupt answer submission from the frontend.
//
// It delegates to InterruptUsecase for business logic, which uses
// the SET+PUBLISH pattern to deliver answers to the waiting interrupt
// handler goroutine (in internal/worker/interrupt_handler.go):
//   - SET with TTL stores the answer durably, so the subscriber can retrieve
//     it via GET even if the pub/sub message is missed.
//   - PUBLISH notifies the subscriber in real time when it is already listening.
//
// The subscriber (handleInterrupt) does SUBSCRIBE then GET to catch answers
// that arrived before the subscription was established.
type InterruptHandler struct {
	interruptUC *usecase.InterruptUsecase
	signer      *auth.JWTSigner
	banChecker  middleware.UserBanChecker
}

// NewInterruptHandler creates an InterruptHandler.
func NewInterruptHandler(interruptUC *usecase.InterruptUsecase, signer *auth.JWTSigner, banChecker middleware.UserBanChecker) *InterruptHandler {
	return &InterruptHandler{interruptUC: interruptUC, signer: signer, banChecker: banChecker}
}

// RegisterRoutes registers interrupt-related routes on the given ServeMux.
//
// The answer endpoint requires JWT authentication and validates session ownership.
// When allowDevBypass is true (development only), requests may use X-User-ID /
// X-Device-ID headers instead of a Bearer token.
func (h *InterruptHandler) RegisterRoutes(mux *http.ServeMux, allowDevBypass bool) {
	authMiddleware := middleware.JWTAuth(h.signer, allowDevBypass, h.banChecker)
	handler := authMiddleware(http.HandlerFunc(h.SubmitAnswer))
	mux.Handle("POST /api/sessions/{sessionID}/interrupts/{interruptID}/answer", handler)
}

// SubmitAnswer receives an interrupt answer from the frontend and delivers it
// to the waiting interrupt handler via Redis SET+PUBLISH.
//
// Route: POST /api/sessions/{sessionID}/interrupts/{interruptID}/answer
// Body:  {"answer": "..."}
//
// Security: Requires JWT authentication and validates session ownership.
func (h *InterruptHandler) SubmitAnswer(w http.ResponseWriter, r *http.Request) {
	// Limit request body size to prevent abuse (1MB)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	sessionIDStr := r.PathValue("sessionID")
	interruptID := r.PathValue("interruptID")

	sessionID, err := uuid.Parse(sessionIDStr)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "interrupt.invalid_session_id", "invalid session ID")
		return
	}
	if interruptID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "interrupt.invalid_interrupt_id", "invalid interrupt ID")
		return
	}

	var req struct {
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "interrupt.invalid_request_body", "invalid request body")
		return
	}
	if req.Answer == "" {
		httputil.WriteError(w, http.StatusBadRequest, "interrupt.empty_answer", "answer must not be empty")
		return
	}
	// Validate answer length to prevent excessive Redis memory usage
	if len(req.Answer) > 10000 {
		httputil.WriteError(w, http.StatusBadRequest, "interrupt.answer_too_long", "answer must be <= 10000 characters")
		return
	}

	ctx := r.Context()

	// Verify session ownership: caller must own the session
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		httputil.WriteError(w, http.StatusUnauthorized, "auth.required", "authentication required")
		return
	}

	if logger.IsDebugMode() {
		logger.Debug(ctx, "[interrupt.HTTP] entry",
			zap.String("session", sessionID.String()),
			zap.String("interrupt", interruptID),
			zap.Int("answer_len", len(req.Answer)))
	}

	// Delegate to use case (handles ownership validation and SET+PUBLISH)
	if err := h.interruptUC.SubmitAnswer(ctx, userID, sessionID, interruptID, req.Answer); err != nil {
		if errors.Is(err, usecase.ErrInterruptSessionNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "interrupt.session_not_found", "session not found")
			return
		}
		if errors.Is(err, usecase.ErrInterruptForbidden) {
			httputil.WriteError(w, http.StatusForbidden, "auth.forbidden", "not authorized for this session")
			return
		}
		if errors.Is(err, usecase.ErrInterruptStoreFailed) {
			logger.Error(ctx, "[interrupt] SET+PUBLISH failed",
				zap.String("session", sessionID.String()),
				zap.String("interrupt", interruptID),
				zap.Error(err))
			httputil.WriteError(w, http.StatusInternalServerError, "interrupt.store_failed", "store answer failed, please retry later")
			return
		}
		// Unexpected error
		logger.Error(ctx, "[interrupt] unexpected error",
			zap.String("session", sessionID.String()),
			zap.String("interrupt", interruptID),
			zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "interrupt.internal_error", "internal error")
		return
	}

	httputil.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
