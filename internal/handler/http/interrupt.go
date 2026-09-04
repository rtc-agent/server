package httphandler

import (
	"encoding/json"
	"net/http"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/pkg/logger"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
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
		logger.Debug("[interrupt.HTTP] entry: session=%s interrupt=%s answer_len=%d stack:\n%s",
			sessionID, interruptID, len(req.Answer), logger.CaptureStack(0))
	}

	// SET+PUBLISH pattern:
	// 1. SET answer with TTL (catches early arrivals before subscriber is ready).
	answerKey := cache.InterruptAnswer(sessionID.String(), interruptID)
	if err := h.redis.Set(ctx, answerKey, req.Answer, h.interruptTTL.InterruptAnswerTTL).Err(); err != nil {
		logger.Error("[interrupt] redis SET failed: session=%s interrupt=%s err=%v", sessionID, interruptID, err)
		httputil.WriteError(w, http.StatusInternalServerError, "interrupt.store_failed", "store answer failed, please retry later")
		return
	} else if logger.DebugMode {
		logger.Debug("[interrupt.HTTP] SET answer: session=%s interrupt=%s key=%s",
			sessionID, interruptID, answerKey)
	}

	// 2. PUBLISH to notify the waiting subscriber.
	channel := cache.InterruptChannel(sessionID.String(), interruptID)
	if err := h.redis.Publish(ctx, channel, req.Answer).Err(); err != nil {
		logger.Error("[interrupt] redis PUBLISH failed: session=%s interrupt=%s err=%v", sessionID, interruptID, err)
		httputil.WriteError(w, http.StatusInternalServerError, "interrupt.publish_failed", "publish failed, please retry later")
		return
	} else if logger.DebugMode {
		logger.Debug("[interrupt.HTTP] PUBLISH answer: session=%s interrupt=%s channel=%s",
			sessionID, interruptID, channel)
	}

	httputil.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
