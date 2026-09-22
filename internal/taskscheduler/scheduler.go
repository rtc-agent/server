// Package taskscheduler provides the asynq-based implementation of TaskScheduler.
//
// The package is named taskscheduler (not asynq) to avoid collision with the
// imported github.com/hibiken/asynq module.
package taskscheduler

import (
	"context"
	"time"

	hibikenasynq "github.com/hibiken/asynq"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

const (
	// LoopQueue is the queue name for loop tasks.
	LoopQueue = "loop"
)

// Impl is the asynq-based implementation of TaskScheduler.
// Exported for use in server initialization (creating loop worker and recovery).
type Impl struct {
	client    *hibikenasynq.Client
	inspector *hibikenasynq.Inspector
}

// NewTaskScheduler creates a TaskScheduler backed by asynq.
func NewTaskScheduler(redisAddr string) (usecase.TaskScheduler, error) {
	client := hibikenasynq.NewClient(hibikenasynq.RedisClientOpt{Addr: redisAddr})
	inspector := hibikenasynq.NewInspector(hibikenasynq.RedisClientOpt{Addr: redisAddr})
	return &Impl{client: client, inspector: inspector}, nil
}

// ScheduleDelayed enqueues a task for execution after the specified delay.
// It returns the asynq task ID, which can be used to cancel the task later.
func (s *Impl) ScheduleDelayed(
	ctx context.Context,
	taskType string,
	payload []byte,
	delay time.Duration,
) (string, error) {
	task := hibikenasynq.NewTask(taskType, payload)
	// Use EnqueueContext to propagate context (including timeout/cancellation and trace information)
	// to the Redis operation, preventing potential goroutine leaks if Redis is unresponsive.
	info, err := s.client.EnqueueContext(ctx, task,
		hibikenasynq.ProcessIn(delay),
		hibikenasynq.Queue(LoopQueue),
		hibikenasynq.MaxRetry(3),
	)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

// Cancel removes a previously enqueued task from the loop queue.
func (s *Impl) Cancel(ctx context.Context, taskID string) error {
	// Inspector.DeleteTask requires the queue name. Loop tasks are in LoopQueue.
	return s.inspector.DeleteTask(LoopQueue, taskID)
}

// Client returns the underlying asynq.Client (used by Worker, Recovery, Cleanup).
func (s *Impl) Client() *hibikenasynq.Client {
	return s.client
}

// Inspector returns the underlying asynq.Inspector.
func (s *Impl) Inspector() *hibikenasynq.Inspector {
	return s.inspector
}

// Close shuts down the asynq client.
func (s *Impl) Close() {
	if err := s.client.Close(); err != nil {
		logger.Error(context.Background(), "asynq client close failed", zap.Error(err))
	}
}
