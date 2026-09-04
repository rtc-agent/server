// Package agent integrates pkg/turn-agent with the rtc-agent application.
//
// This package replaces internal/worker as the bridge between rtc-queue's
// Worker and the eino agent. Unlike the old worker package, which maintained
// long-lived SessionActor objects with channels and goroutines, this package
// is stateless: each Agent.Process call handles one work item from rtc-queue.
//
// The integration is done via the New function, which constructs a
// turnagent.Config with all callbacks wired to the application's DB, Redis,
// publisher, and chat model. The returned *turnagent.Agent can be used
// directly as the OnWork callback of an rtcqueue.Worker.
//
// # Migration from internal/worker
//
// Old pattern (stateful):
//
//	manager := worker.NewManager(deps, cfg)
//	manager.Start(ctx)
//	// SessionActor per session, channels for interrupt/resume
//
// New pattern (stateless):
//
//	a, err := agent.New(agent.Config{...})
//	w := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{OnWork: a.Process})
//	go w.Run(ctx)
//
// Key differences:
//   - No SessionActor: each Process call is independent
//   - Turn ownership moves from API layer to callbacks (CreateTurn/LookupTurn)
//   - No stream consumption in app: turn-agent handles eino stream internally
//   - Interrupt handling: InterruptTurn callback → app publishes Resume work item
package agent

import (
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/usecase"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/cloudwego/eino/adk"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/trace"
)

// Config holds the dependencies needed to build a turnagent.Agent.
//
// All fields are required unless marked optional.
type Config struct {
	// Deps provides the application's repositories, publisher, chat model,
	// and system prompt. Shared with the old worker package.
	Deps *usecase.Dependencies

	// Redis is the Redis client used for checkpoint storage and stream
	// message chunk buffering. Required.
	Redis redis.UniversalClient

	// Logger provides structured logging for the turn-agent runtime.
	// Optional — if nil, no logs are emitted.
	Logger turnagent.Logger

	// Tracer provides OpenTelemetry distributed tracing.
	// Optional — if nil, no spans are created.
	Tracer trace.Tracer

	// Metrics provides pluggable metrics collection.
	// Optional — if nil, no metrics are emitted.
	Metrics turnagent.Metrics

	// ContextTokensLimit is the token threshold for triggering summarization.
	// If <= 0, defaults to 25000.
	ContextTokensLimit int

	// CancelGracePeriod controls graceful vs. immediate cancellation.
	// If zero (default), cancellation is immediate. If positive, the agent
	// runs until a safe point with the given grace period as upper bound.
	CancelGracePeriod time.Duration

	// EnableLLMLogging hints that verbose LLM I/O logging is desired.
	EnableLLMLogging bool

	// CheckpointTTL controls how long eino checkpoints live in Redis.
	// If <= 0, defaults to 24h.
	CheckpointTTL time.Duration

	// StreamChunkTTL controls how long streaming message chunks live in Redis.
	// If <= 0, defaults to 5m.
	StreamChunkTTL time.Duration
}

// New constructs a *turnagent.Agent with all callbacks wired to the
// application's dependencies. The returned agent can be used directly as
// rtcqueue.WorkerConfig.OnWork.
//
// The callbacks close over the Config's dependencies and use the helper
// methods defined in callbacks.go, data.go, checkpoint.go, and summarize.go.
func New(cfg Config) (*turnagent.Agent, error) {
	if cfg.Deps == nil {
		return nil, fmt.Errorf("agent: Config.Deps is required")
	}
	if cfg.Redis == nil {
		return nil, fmt.Errorf("agent: Config.Redis is required")
	}

	// Build a helpers struct that holds all shared state for the callbacks.
	// The helpers struct provides methods that match the turnagent callback
	// signatures, closing over the Config's dependencies.
	h := &helpers{
		deps:               cfg.Deps,
		rdb:                cfg.Redis,
		logger:             cfg.Logger,
		tracer:             cfg.Tracer,
		metrics:            cfg.Metrics,
		contextTokensLimit: cfg.ContextTokensLimit,
		enableLLMLogging:   cfg.EnableLLMLogging,
		streamChunkTTL:     defaultStreamChunkTTL(cfg.StreamChunkTTL),
	}

	if h.contextTokensLimit <= 0 {
		h.contextTokensLimit = 25000
	}

	// Build the summarization middleware. It is created once and shared
	// across all turns (it is stateless — the per-turn state lives in the
	// CompressContext / OnCompress closures).
	summarizeMW, err := h.buildSummarizationMiddleware()
	if err != nil {
		return nil, fmt.Errorf("agent: build summarization middleware: %w", err)
	}

	// Build the turnagent.Config with all callbacks.
	taCfg := turnagent.Config{
		// Turn ownership
		CreateTurn: h.createTurn,
		LookupTurn: h.lookupTurn,

		// Lifecycle
		BeginTurn:     h.beginTurn,
		CompleteTurn:  h.completeTurn,
		InterruptTurn: h.interruptTurn,
		ResumeTurn:    h.resumeTurn,
		FailTurn:      h.failTurn,
		CancelTurn:    h.cancelTurn,

		// Data
		LoadMessages: h.loadMessages,
		CreateTools:  h.createTools,
		CreateAgent:  h.createAgent,
		PublishEvent: h.publishEvent,

		// Checkpoint
		CheckpointStore: newRedisCheckpointStore(cfg.Redis, defaultCheckpointTTL(cfg.CheckpointTTL)),

		// DeriveCheckpointID overrides the default key pattern to maintain
		// backward compatibility with existing checkpoints in Redis.
		// The default would produce "checkpoint:turnagent:session:{sessionID}"
		// but old checkpoints are stored at "checkpoint:session:{sessionID}".
		// Using the old pattern avoids breaking in-flight turns on upgrade.
		DeriveCheckpointID: func(sessionID string) string {
			return cache.Checkpoint("session:" + sessionID)
		},

		// Middleware — the summarization middleware is injected into the
		// agent by CreateAgent via this field.
		AgentMiddlewares: []adk.ChatModelAgentMiddleware{summarizeMW},

		// Observability
		Logger:           cfg.Logger,
		Tracer:           cfg.Tracer,
		Metrics:          cfg.Metrics,
		EnableLLMLogging: cfg.EnableLLMLogging,

		// Cancel
		Cancel: turnagent.CancelConfig{
			GracePeriod: cfg.CancelGracePeriod,
		},
	}

	return turnagent.New(taCfg)
}

// helpers holds the shared dependencies and state for all callbacks.
// It is the "integration struct" that replaces the old worker.Manager's
// role as the bridge between the agent runtime and the application.
//
// Method naming convention: methods named after turnagent callbacks
// (createTurn, beginTurn, etc.) are the callback implementations. Methods
// with other names (publishTurnEvent, loadSession) are internal helpers.
type helpers struct {
	deps               *usecase.Dependencies
	rdb                redis.UniversalClient
	logger             turnagent.Logger
	tracer             trace.Tracer
	metrics            turnagent.Metrics
	contextTokensLimit int
	enableLLMLogging   bool
	streamChunkTTL     time.Duration

	// summarizeMW is the summarization middleware, created once in New().
	// Stored on helpers so CreateAgent can close over it without capturing
	// the entire helpers struct.
	summarizeMW adk.ChatModelAgentMiddleware

	// streamState tracks per-turn streaming message state.
	// Key: turnID (string), Value: *turnStreamState.
	//
	// The old code tracked this state in the handleEvents closure, which was
	// created fresh per session. In the new stateless model, we need a
	// concurrent-safe map keyed by turnID since multiple turns may execute
	// concurrently on the same helpers.
	streamState streamStateMap
}

// defaultCheckpointTTL returns the configured checkpoint TTL, defaulting to 24h.
func defaultCheckpointTTL(d time.Duration) time.Duration {
	if d <= 0 {
		return 24 * time.Hour
	}
	return d
}

// defaultStreamChunkTTL returns the configured stream chunk TTL, defaulting to 5m.
func defaultStreamChunkTTL(d time.Duration) time.Duration {
	if d <= 0 {
		return 5 * time.Minute
	}
	return d
}
