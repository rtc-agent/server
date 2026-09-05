// Package usecase 包含业务逻辑的 UseCase 层。
//
// UseCase 层是协议无关的：它不知道调用方是 RPC、HTTP 还是 LLM，
// 只关心业务规则和事务一致性。这样多个协议层可以复用同一份业务逻辑。
package usecase

import (
	"context"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/cloudwego/eino/components/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Publisher 定义 UpdatePublisher 的发布接口，便于测试 mock。
// *updates.UpdatePublisher 实现了此接口。
type Publisher interface {
	Publish(ctx context.Context, items ...updates.UpdatePublishItem) ([]*protocol.Update, error)
	RunAndPublish(ctx context.Context, fn func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error)
}

// Dependencies UseCase 层所需的依赖。
//
// 仅持有 repos 和基础设施，不包含业务逻辑。
// 由 server 层在启动时构造并注入。
type Dependencies struct {
	DB              *gorm.DB
	Redis           redis.UniversalClient
	SessionRepo     repo.SessionRepo
	MessageRepo     repo.MessageRepo
	TurnRepo        repo.TurnRepo
	RtcRepo         repo.RtcRepo
	UpdatePublisher Publisher

	// ChatModel is the eino ChatModel for LLM interactions.
	// Required for agent execution in turn-loop sessions.
	ChatModel model.ToolCallingChatModel

	// LLMConfig provides access to LLM-level configuration (retry, etc.)
	LLMConfig config.LLMConfig

	// SystemPrompt is the agent's instruction/system message.
	// Defines the agent's behavior and capabilities.
	SystemPrompt string

	// WorkerConfig provides access to worker-level configuration (TTLs, etc.)
	WorkerConfig config.WorkerConfig
}
