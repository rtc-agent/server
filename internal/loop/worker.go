// Package loop implements the Loop command's background task processing.
//
// Worker handles asynq tasks for loop execution, Recovery monitors for stale
// or expired loops, and Cleanup handles session close cleanup.
package loop

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	hibikenasynq "github.com/hibiken/asynq"
	"github.com/rtc-agent/server/internal/repo"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// Worker processes loop asynq tasks.
type Worker struct {
	queue    *rtcqueue.Queue
	loopRepo repo.LoopRepo
}

// NewWorker creates a loop worker.
func NewWorker(queue *rtcqueue.Queue, loopRepo repo.LoopRepo) *Worker {
	return &Worker{
		queue:    queue,
		loopRepo: loopRepo,
	}
}

// HandleLoopTask processes loop:execute tasks.
//
// Idempotency: safe to call multiple times. If the loop no longer exists,
// is not active, or the ID doesn't match, returns nil without side effects.
func (w *Worker) HandleLoopTask(ctx context.Context, t *hibikenasynq.Task) error {
	var payload struct {
		SessionID string `json:"session_id"`
		LoopID    string `json:"loop_id"`
	}

	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}

	sessionUUID, err := uuid.Parse(payload.SessionID)
	if err != nil {
		return fmt.Errorf("parse session ID: %w", err)
	}
	loopUUID, err := uuid.Parse(payload.LoopID)
	if err != nil {
		return fmt.Errorf("parse loop ID: %w", err)
	}

	// 1. Validate loop still exists and is active
	loop, err := w.loopRepo.FindActive(ctx, sessionUUID)
	if err != nil {
		return fmt.Errorf("find active loop: %w", err)
	}
	if loop == nil {
		// Loop was cancelled/paused, no longer executing
		return nil
	}
	if loop.ID != loopUUID {
		// Loop ID doesn't match (may have been replaced by a new loop)
		return nil
	}

	// 2. Submit to rtc-queue for execution (only SessionID, consistent with Goal pattern)
	workPayload, err := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: payload.SessionID,
	})
	if err != nil {
		return fmt.Errorf("marshal work payload: %w", err)
	}

	_, err = w.queue.Publish(ctx, payload.SessionID, string(workPayload), rtcqueue.SubmitWorkPriority)
	if err != nil {
		return fmt.Errorf("publish to rtc-queue: %w", err)
	}

	return nil
}

// RegisterHandlers registers asynq handlers.
func (w *Worker) RegisterHandlers(mux *hibikenasynq.ServeMux) {
	mux.HandleFunc(LoopTaskType, w.HandleLoopTask)
}

// StartAsynqServer starts the asynq server.
func StartAsynqServer(redisAddr string, worker *Worker) error {
	srv := hibikenasynq.NewServer(
		hibikenasynq.RedisClientOpt{Addr: redisAddr},
		hibikenasynq.Config{
			Concurrency: 10,
			Queues: map[string]int{
				"loop":    6,
				"default": 3,
			},
		},
	)

	mux := hibikenasynq.NewServeMux()
	worker.RegisterHandlers(mux)

	return srv.Run(mux)
}
