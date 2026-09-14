// Package taskscheduler provides the asynq-based implementation of TaskScheduler.
//
// 包名使用 taskscheduler 而非 asynq，避免与导入的 github.com/hibiken/asynq 冲突。
package taskscheduler

import (
	"context"
	"time"

	hibikenasynq "github.com/hibiken/asynq"
	"github.com/rtc-agent/server/internal/usecase"
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

func (s *Impl) ScheduleDelayed(
	ctx context.Context,
	taskType string,
	payload []byte,
	delay time.Duration,
) (string, error) {
	task := hibikenasynq.NewTask(taskType, payload)
	info, err := s.client.Enqueue(task,
		hibikenasynq.ProcessIn(delay),
		hibikenasynq.Queue(LoopQueue),
		hibikenasynq.MaxRetry(3),
	)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

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
	_ = s.client.Close()
}
