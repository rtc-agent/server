// Package usecase 包含业务逻辑的 UseCase 层。
//
// UseCase 层是协议无关的：它不知道调用方是 RPC、HTTP 还是 LLM，
// 只关心业务规则和事务一致性。这样多个协议层可以复用同一份业务逻辑。
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

// Publisher 定义 UpdatePublisher 的发布接口，便于测试 mock。
// *updates.UpdatePublisher 实现了此接口。
type Publisher interface {
	Publish(ctx context.Context, items ...updates.UpdatePublishItem) ([]*protocol.Update, error)
	RunAndPublish(ctx context.Context, fn func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error)
	ResolveMessageContent(msg *model.Message) string
}

// Dependencies UseCase 层所需的依赖。
//
// 仅持有 repos 和基础设施，不包含业务逻辑。
// 由 server 层在启动时构造并注入。
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

// TaskScheduler 延迟任务调度接口
//
// 用于 Loop 命令的定时触发。具体实现在批次三提供（基于 asynq）。
// 批次二通过 nil 检查实现优雅降级。
type TaskScheduler interface {
	// ScheduleDelayed 调度一个延迟任务，返回任务 ID
	ScheduleDelayed(ctx context.Context, taskType string, payload []byte, delay time.Duration) (taskID string, err error)

	// Cancel 取消一个已调度的任务
	Cancel(ctx context.Context, taskID string) error
}
