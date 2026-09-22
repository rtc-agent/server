// Package usecase contains the UseCase layer for business logic.
//
// The UseCase layer is protocol-agnostic: it does not know whether the caller
// is RPC, HTTP, or LLM. It only cares about business rules and transactional
// consistency. This allows multiple protocol layers to reuse the same business
// logic.
package usecase

import (
	"context"
	"time"

	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/cloudwego/eino/callbacks"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Publisher defines the publishing interface for UpdatePublisher, facilitating
// test mocking. *updates.UpdatePublisher implements this interface.
type Publisher interface {
	Publish(ctx context.Context, items ...updates.UpdatePublishItem) ([]*protocol.Update, error)
	RunAndPublish(ctx context.Context, fn func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error)
	ResolveMessageContent(msg *model.Message) string
}

// Dependencies holds the dependencies required by the UseCase layer.
//
// Contains only repos and infrastructure — no business logic.
// Constructed and injected by the server layer at startup.
type Dependencies struct {
	DB                *gorm.DB
	Redis             redis.UniversalClient
	SessionRepo       repo.SessionRepo
	MessageRepo       repo.MessageRepo
	TurnRepo          repo.TurnRepo
	RtcRepo           repo.RtcRepo
	GoalRepo          repo.GoalRepo
	LoopRepo          repo.LoopRepo
	SessionMemoryRepo repo.SessionMemoryRepo
	UserMemoryRepo    repo.UserMemoryRepo
	UpdatePublisher   Publisher

	// ChatModel is the eino ChatModel for LLM interactions.
	// Required for agent execution in turn-loop sessions.
	ChatModel einomodel.ToolCallingChatModel

	// LLMConfig provides access to LLM-level configuration (retry, etc.)
	LLMConfig config.LLMConfig

	// SystemPrompt is the agent's instruction/system message.
	// Defines the agent's behavior and capabilities.
	SystemPrompt string

	// WorkerConfig provides access to worker-level configuration (TTLs, etc.)
	WorkerConfig config.WorkerConfig

	// CommandRegistry is the slash-command framework. It owns prompt
	// injection, tool collection, and turn hooks for all registered
	// commands (e.g., /goal, /persona).
	CommandRegistry *command.CommandRegistry

	// TokenCallbackHandler is the eino callback handler for recording LLM token
	// usage. Set by agent.New() after construction. Background LLM calls
	// (title summarization, memory extraction) inject this into their context
	// via callbacks.InitCallbacks so token consumption is tracked to Session.TotalTokens.
	TokenCallbackHandler callbacks.Handler

	// TaskScheduler provides delayed task scheduling for Loop command.
	// Used by LoopWorkflow.OnTurnComplete to enqueue the next loop turn.
	// Actual implementation is provided in batch 3; nil checks are used
	// in batch 2 for graceful degradation.
	TaskScheduler TaskScheduler
}

// TaskScheduler is the interface for delayed task scheduling.
//
// Used by the Loop command for timed triggers. The concrete implementation
// is provided in batch 3 (based on asynq). Batch 2 uses nil checks for
// graceful degradation.
type TaskScheduler interface {
	// ScheduleDelayed schedules a delayed task and returns the task ID.
	// If taskID is empty, asynq will generate a unique ID.
	// If taskID is provided and a task with that ID already exists (pending/processing),
	// asynq returns ErrTaskIDConflict, making this operation idempotent.
	ScheduleDelayed(ctx context.Context, taskType string, payload []byte, delay time.Duration, taskID string) (string, error)

	// Cancel cancels a previously scheduled task.
	Cancel(ctx context.Context, taskID string) error
}
