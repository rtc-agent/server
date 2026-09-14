package httphandler

import (
	"encoding/json"
	"net/http"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// InterruptHandler handles interrupt answer submission from the frontend.
//
// It uses the SET+PUBLISH pattern to deliver answers to the waiting interrupt
// handler goroutine (in internal/worker/interrupt_handler.go):
//   - SET with TTL stores the answer durably, so the subscriber can retrieve
//     it via GET even if the pub/sub message is missed.
//   - PUBLISH notifies the subscriber in real time when it is already listening.
//
// The subscriber (handleInterrupt) does SUBSCRIBE then GET to catch answers
// that arrived before the subscription was established.
type InterruptHandler struct {
	redis       redis.UniversalClient
	workerCfg   config.WorkerConfig
	sessionRepo repo.SessionRepo
	signer      *auth.JWTSigner
}

// NewInterruptHandler creates an InterruptHandler.
func NewInterruptHandler(redis redis.UniversalClient, workerCfg config.WorkerConfig, sessionRepo repo.SessionRepo, signer *auth.JWTSigner) *InterruptHandler {
	return &InterruptHandler{redis: redis, workerCfg: workerCfg, sessionRepo: sessionRepo, signer: signer}
}

// RegisterRoutes registers interrupt-related routes on the given ServeMux.
//
// The answer endpoint requires JWT authentication and validates session ownership.
// When allowDevBypass is true (development only), requests may use X-User-ID /
// X-Device-ID headers instead of a Bearer token.
func (h *InterruptHandler) RegisterRoutes(mux *http.ServeMux, allowDevBypass bool) {
	authMiddleware := middleware.JWTAuth(h.signer, allowDevBypass)
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

	session, err := h.sessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		if repo.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "interrupt.session_not_found", "session not found")
		} else {
			logger.Error(ctx, "[interrupt] failed to get session",
				zap.String("session", sessionID.String()),
				zap.Error(err))
			httputil.WriteError(w, http.StatusInternalServerError, "interrupt.db_error", "failed to get session")
		}
		return
	}

	if session.OwnerRefID != userID.String() {
		httputil.WriteError(w, http.StatusForbidden, "auth.forbidden", "not authorized for this session")
		return
	}

	if logger.DebugMode {
		logger.Debug(ctx, "[interrupt.HTTP] entry",
			zap.String("session", sessionID.String()),
			zap.String("interrupt", interruptID),
			zap.Int("answer_len", len(req.Answer)))
	}

	// 原子执行 SET + PUBLISH（Lua 脚本保证一致性）：
	// 1. SET answer with TTL（catches early arrivals before subscriber is ready）
	// 2. PUBLISH to notify the waiting subscriber
	answerKey := cache.InterruptAnswer(sessionID.String(), interruptID)
	channel := cache.InterruptChannel(sessionID.String(), interruptID)
	ttlSeconds := int(h.workerCfg.InterruptAnswerTTL.Seconds())

	if err := cache.InterruptSetPublish.Run(ctx, h.redis,
		[]string{answerKey, channel},
		req.Answer, ttlSeconds,
	).Err(); err != nil {
		logger.Error(ctx, "[interrupt] SET+PUBLISH failed",
			zap.String("session", sessionID.String()),
			zap.String("interrupt", interruptID),
			zap.Error(err))
		httputil.WriteError(w, http.StatusInternalServerError, "interrupt.store_failed", "store answer failed, please retry later")
		return
	}

	if logger.DebugMode {
		logger.Debug(ctx, "[interrupt.HTTP] SET+PUBLISH answer",
			zap.String("session", sessionID.String()),
			zap.String("interrupt", interruptID),
			zap.String("key", answerKey),
			zap.String("channel", channel))
	}

	httputil.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
