package httphandler

import (
	"encoding/json"
	"net/http"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/httputil"
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
	redis        redis.UniversalClient
	interruptTTL config.WorkerConfig
}

// NewInterruptHandler creates an InterruptHandler.
func NewInterruptHandler(redis redis.UniversalClient, workerCfg config.WorkerConfig) *InterruptHandler {
	return &InterruptHandler{redis: redis, interruptTTL: workerCfg}
}

// RegisterRoutes registers interrupt-related routes on the given ServeMux.
func (h *InterruptHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/sessions/{sessionID}/interrupts/{interruptID}/answer",
		h.SubmitAnswer)
}

// SubmitAnswer receives an interrupt answer from the frontend and delivers it
// to the waiting interrupt handler via Redis SET+PUBLISH.
//
// Route: POST /api/sessions/{sessionID}/interrupts/{interruptID}/answer
// Body:  {"answer": "..."}
func (h *InterruptHandler) SubmitAnswer(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()

	if logger.DebugMode {
		logger.Debug(ctx, "[interrupt.HTTP] entry",
			zap.String("session", sessionID.String()),
			zap.String("interrupt", interruptID),
			zap.Int("answer_len", len(req.Answer)),
			zap.String("stack", logger.CaptureStack(0)))
	}

	// 原子执行 SET + PUBLISH（Lua 脚本保证一致性）：
	// 1. SET answer with TTL（catches early arrivals before subscriber is ready）
	// 2. PUBLISH to notify the waiting subscriber
	answerKey := cache.InterruptAnswer(sessionID.String(), interruptID)
	channel := cache.InterruptChannel(sessionID.String(), interruptID)
	ttlSeconds := int(h.interruptTTL.InterruptAnswerTTL.Seconds())

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
