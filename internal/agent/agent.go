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
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
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

	// Queue is the rtc-queue for publishing work items (submit/resume/compact).
	// Required for Sub Agent support (triggering parent session resume).
	Queue *rtcqueue.Queue

	// WorkerID identifies this worker instance. Used for session lock claims.
	// Required.
	WorkerID string

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

	// AutoCompactBufferTokens is the buffer token count for auto-compaction.
	// Used to calculate the actual trigger threshold: ContextTokensLimit - AutoCompactBufferTokens.
	// If <= 0, defaults to 13000.
	AutoCompactBufferTokens int

	// CacheHitRateWarnThreshold is the cache hit rate warning threshold.
	// Session cumulative cache hit rate = TotalCachedReadTokens / (TotalCachedReadTokens + TotalInputTokens).
	// When below this threshold, a warn log is emitted. Negative value disables the alert.
	// If 0 (not set), defaults to 0.88 (88%).
	CacheHitRateWarnThreshold float64

	// MaxOutputTokensForSummary is the maximum output tokens for summarization.
	// If <= 0, defaults to 20000.
	MaxOutputTokensForSummary int

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
	// If <= 0, defaults to 15m. The longer TTL handles extended LLM thinking/reasoning
	// streams that may exceed shorter durations, preventing chunk loss and incomplete
	// message persistence.
	StreamChunkTTL time.Duration

	// MicrocompactGapMinutes is the idle time threshold (in minutes) for
	// time-based Microcompact. If <= 0, defaults to 60.
	MicrocompactGapMinutes int

	// MicrocompactKeepRecent is the number of recent tool results to keep
	// during Microcompact. If <= 0, defaults to 5.
	MicrocompactKeepRecent int

	// ToolResultBudgetMaxTokens is the maximum tokens for a single tool result.
	// If <= 0, defaults to 10000 (≈40KB). This prevents oversized tool outputs
	// from consuming excessive context space.
	ToolResultBudgetMaxTokens int

	// EnableStrategicCacheBreakpoints enables strategic cache breakpoints to
	// protect stable content from invalidation caused by microcompact or tool
	// result budget modifications. Default: true.
	//
	// When enabled, cache breakpoints are set at:
	//   - bp1: summary message (protects system + attachments + summary)
	//   - bp2: last conversation message (protects latest context)
	//
	// This improves cache hit rate from 0% to 75% after microcompact, reducing
	// input costs by ~69% in those scenarios.
	EnableStrategicCacheBreakpoints bool

	// ModelPricing is the model pricing configuration (optional, used for cost calculation).
	// Uses default pricing (Claude 3.5 Sonnet) when not configured.
	ModelPricing *ModelPricingConfig

	// ShowRawErrors controls whether RawError content is visible in error messages.
	// Typically derived from infraCfg.Debug.Enabled && infraCfg.Debug.ShowRawErrors.
	// When false (default), error messages still store sanitized RawError but the
	// frontend is instructed not to render it.
	ShowRawErrors bool
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
	if cfg.Queue == nil {
		return nil, fmt.Errorf("agent: Config.Queue is required")
	}
	if cfg.WorkerID == "" {
		// Auto-generate WorkerID if not provided
		cfg.WorkerID = fmt.Sprintf("worker-%s", uuid.Must(uuid.NewV7()).String()[:8])
	}

	// Build a helpers struct that holds all shared state for the callbacks.
	// The helpers struct provides methods that match the turnagent callback
	// signatures, closing over the Config's dependencies.
	loggerImpl := cfg.Logger
	if loggerImpl == nil {
		loggerImpl = logger.NoopLogger{}
	}
	h := &helpers{
		deps:                            cfg.Deps,
		rdb:                             cfg.Redis,
		queue:                           cfg.Queue,
		logger:                          loggerImpl,
		tracer:                          cfg.Tracer,
		metrics:                         cfg.Metrics,
		contextTokensLimit:              cfg.ContextTokensLimit,
		autoCompactBufferTokens:         cfg.AutoCompactBufferTokens,
		cacheHitRateWarnThreshold:       cfg.CacheHitRateWarnThreshold,
		maxOutputTokensForSummary:       cfg.MaxOutputTokensForSummary,
		enableLLMLogging:                cfg.EnableLLMLogging,
		streamChunkTTL:                  defaultStreamChunkTTL(cfg.StreamChunkTTL),
		microcompactGapMinutes:          cfg.MicrocompactGapMinutes,
		microcompactKeepRecent:          cfg.MicrocompactKeepRecent,
		toolResultBudgetMaxTokens:       cfg.ToolResultBudgetMaxTokens,
		enableStrategicCacheBreakpoints: cfg.EnableStrategicCacheBreakpoints,
		showRawErrors:                   cfg.ShowRawErrors,
	}

	applyHelperDefaults(h)

	// Initialize helpers subsystems (token estimator, commands, attachments, etc.)
	if err := h.initialize(cfg); err != nil {
		return nil, err
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
		CheckpointStore: newMetricsCheckpointStore(
			newRedisCheckpointStore(cfg.Redis, defaultCheckpointTTL(cfg.CheckpointTTL)),
			cfg.Metrics,
		),

		// DeriveCheckpointID overrides the default key pattern to maintain
		// backward compatibility with existing checkpoints in Redis.
		// The default would produce "checkpoint:turnagent:session:{sessionID}"
		// but old checkpoints are stored at "checkpoint:session:{sessionID}".
		// Using the old pattern avoids breaking in-flight turns on upgrade.
		DeriveCheckpointID: func(sessionID string) string {
			return cache.Checkpoint("session:" + sessionID)
		},

		// Middleware — merge assistant middleware (merges adjacent assistant messages)
		// and summarization middleware (context compression)
		// Order matters: merge first, then summarize
		AgentMiddlewares: []adk.ChatModelAgentMiddleware{h.mergeAssistantMW, h.summarizeMW},

		// eino Callbacks — the token usage handler records metrics and logs
		// for every ChatModel call (including summarizeMessages).
		// Uses the handler stored on helpers so the compact flow can reuse it.
		Callbacks: []callbacks.Handler{
			h.tokenCallbackHandler,
		},

		// Observability
		Logger:           cfg.Logger,
		Tracer:           cfg.Tracer,
		Metrics:          cfg.Metrics,
		EnableLLMLogging: cfg.EnableLLMLogging,

		// Cancel
		Cancel: turnagent.CancelConfig{
			GracePeriod: cfg.CancelGracePeriod,
		},

		// Reactive compact — recover from prompt too long errors
		RecoverFromPromptTooLong:   h.recoverFromPromptTooLong,
		MaxReactiveCompactAttempts: 3,

		// InsertFeedbackMessage bridges the reactive compact path to the
		// application's error message insertion (insertErrorMessage uses
		// uuid.UUID parameters, so we wrap it here).
		InsertFeedbackMessage: func(ctx context.Context, sessionID, turnID, category, title, message string, retryable bool, rawError string) error {
			sid, parseErr := uuid.Parse(sessionID)
			if parseErr != nil {
				return fmt.Errorf("insertFeedbackMessage: invalid session ID %q: %w", sessionID, parseErr)
			}
			tid, parseErr := uuid.Parse(turnID)
			if parseErr != nil {
				return fmt.Errorf("insertFeedbackMessage: invalid turn ID %q: %w", turnID, parseErr)
			}
			return h.insertErrorMessage(ctx, sid, &tid, protocol.ErrorCategory(category), title, message, retryable, rawError)
		},

		// Explicit compact — process compact work items
		CompactContext: h.processCompactWorker,
	}

	return turnagent.New(taCfg, cfg.Queue, cfg.WorkerID)
}

// helpers holds the shared dependencies and state for all callbacks.
// It is the "integration struct" that replaces the old worker.Manager's
// role as the bridge between the agent runtime and the application.
//
// Method naming convention: methods named after turnagent callbacks
// (createTurn, beginTurn, etc.) are the callback implementations. Methods
// with other names (publishTurnEvent, loadSession) are internal helpers.
type helpers struct {
	deps                      *usecase.Dependencies
	rdb                       redis.UniversalClient
	queue                     *rtcqueue.Queue
	logger                    turnagent.Logger
	tracer                    trace.Tracer
	metrics                   turnagent.Metrics
	contextTokensLimit        int
	autoCompactBufferTokens   int
	cacheHitRateWarnThreshold float64
	maxOutputTokensForSummary int
	enableLLMLogging          bool
	streamChunkTTL            time.Duration
	microcompactGapMinutes    int
	microcompactKeepRecent    int
	toolResultBudgetMaxTokens int

	// enableStrategicCacheBreakpoints enables strategic cache breakpoints to
	// protect stable content from microcompact/tool budget invalidation.
	// Set by Config.EnableStrategicCacheBreakpoints (default: true).
	enableStrategicCacheBreakpoints bool

	// Token estimation related.
	tokenEstimator      *TokenEstimator
	tokenUpdateThrottle *Throttle
	modelPricing        ModelPricing

	// summarizeMW is the summarization middleware, created once in New().
	// Stored on helpers so CreateAgent can close over it without capturing
	// the entire helpers struct.
	summarizeMW adk.ChatModelAgentMiddleware

	// mergeAssistantMW is the merge assistant middleware, created once in New().
	// Merges adjacent assistant messages before each ChatModel invocation to
	// prevent cache invalidation in the ReAct loop.
	mergeAssistantMW adk.ChatModelAgentMiddleware

	// streamState tracks per-turn streaming message state.
	// Key: turnID (string), Value: *turnStreamState.
	//
	// The old code tracked this state in the handleEvents closure, which was
	// created fresh per session. In the new stateless model, we need a
	// concurrent-safe map keyed by turnID since multiple turns may execute
	// concurrently on the same helpers.
	streamState streamStateMap

	// attachmentManager coordinates the building and injection of all
	// dynamic attachments (TodoList, SessionMemory, UserMemory).
	// Created once in New() and shared across all turns.
	attachmentManager *AttachmentManager

	// persistedSummaryMsgIDs tracks summary messages that have been persisted
	// during compression. Used to prevent double persistence by OnCompress.
	// Key: sessionID (string), Value: summaryMsgID (string).
	// Concurrent-safe since compressions are sequential per session.
	persistedSummaryMsgIDs sync.Map

	// tokenCallbackHandler is the eino callback handler for recording LLM token
	// usage. Stored on helpers so it can be reused by the compact flow (which
	// bypasses the turn loop and must initialize callbacks itself).
	tokenCallbackHandler callbacks.Handler

	// showRawErrors controls whether RawError content is visible in error messages.
	// Derived from Config.ShowRawErrors at construction time. Typically true only
	// in debug environments (cfg.Debug.Enabled && cfg.Debug.ShowRawErrors).
	// When false (default), sanitized RawError is still stored but the frontend
	// is instructed not to render it.
	showRawErrors bool
}

// applyHelperDefaults fills in zero-value fields on helpers with sensible
// defaults. Extracted from New() to reduce its length.
func applyHelperDefaults(h *helpers) {
	// Default tracer: noop tracer if not provided
	if h.tracer == nil {
		h.tracer = noop.NewTracerProvider().Tracer("rtc-agent")
	}

	if h.contextTokensLimit <= 0 {
		h.contextTokensLimit = 25000
	}
	if h.autoCompactBufferTokens <= 0 {
		h.autoCompactBufferTokens = 13000
	}
	// Default cache hit rate threshold: 88%. Negative disables the alert.
	if h.cacheHitRateWarnThreshold == 0 {
		h.cacheHitRateWarnThreshold = 0.88
	}
	if h.maxOutputTokensForSummary <= 0 {
		h.maxOutputTokensForSummary = 20000
	}
	if h.microcompactGapMinutes <= 0 {
		h.microcompactGapMinutes = 60
	}
	if h.microcompactKeepRecent <= 0 {
		h.microcompactKeepRecent = 5
	}
	if h.toolResultBudgetMaxTokens <= 0 {
		h.toolResultBudgetMaxTokens = DefaultToolResultMaxTokens
	}
}

// initialize sets up the helpers subsystems: token estimator, slash commands,
// attachment manager, summarization middleware, and token callback handler.
// Extracted from New() to reduce its length.
func (h *helpers) initialize(cfg Config) error {
	// Token estimation
	h.tokenEstimator = NewTokenEstimator(h.contextTokensLimit, h.autoCompactBufferTokens, cfg.Deps.SessionRepo)
	h.tokenUpdateThrottle = NewThrottle(500 * time.Millisecond)
	h.modelPricing = NewModelPricing(cfg.ModelPricing)

	// Register built-in slash commands
	registerGoalCommand(cfg.Deps.CommandRegistry, h)
	registerLoopCommand(cfg.Deps.CommandRegistry, h)
	if cfg.Deps.CommandRegistry != nil {
		for _, cmd := range builtinCommands() {
			cfg.Deps.CommandRegistry.Register(cmd)
		}
	}

	// Attachment manager
	h.attachmentManager = NewAttachmentManager(
		[]Attachment{
			NewSessionMemoryAttachment(h),
			NewUserMemoryAttachment(h),
		},
		cfg.Metrics,
		h.logger,
		AttachmentManagerConfig{
			MaxTokensPerAttachment: 5000,
			TotalBudget:            15000,
		},
	)

	// Summarization middleware
	summarizeMW, err := h.buildSummarizationMiddleware()
	if err != nil {
		return fmt.Errorf("agent: build summarization middleware: %w", err)
	}
	h.summarizeMW = summarizeMW

	// Merge assistant middleware
	h.mergeAssistantMW = turnagent.NewMergeAssistantMiddleware(&turnagent.MergeAssistantMiddlewareConfig{
		Log: h.logger,
	})

	// Token callback handler
	h.tokenCallbackHandler = h.newTokenUsageCallbackHandler()
	cfg.Deps.TokenCallbackHandler = h.tokenCallbackHandler

	return nil
}

// defaultCheckpointTTL returns the configured checkpoint TTL, defaulting to 24h.
func defaultCheckpointTTL(d time.Duration) time.Duration {
	if d <= 0 {
		return 24 * time.Hour
	}
	return d
}

// defaultStreamChunkTTL returns the configured stream chunk TTL, defaulting to 15m.
// Increased from 5m to handle long-running LLM thinking/reasoning streams that may
// exceed the original TTL, causing chunk loss and incomplete message persistence.
func defaultStreamChunkTTL(d time.Duration) time.Duration {
	if d <= 0 {
		return 15 * time.Minute
	}
	return d
}

// microcompactConfig returns the MicrocompactConfig derived from the helpers'
// configuration fields.
func (h *helpers) microcompactConfig() MicrocompactConfig {
	return MicrocompactConfig{
		GapThresholdMinutes: h.microcompactGapMinutes,
		KeepRecent:          h.microcompactKeepRecent,
	}
}

// toolResultBudgetConfig returns the ToolResultBudgetConfig derived from the
// helpers' configuration fields.
func (h *helpers) toolResultBudgetConfig() ToolResultBudgetConfig {
	return ToolResultBudgetConfig{
		MaxTokens: h.toolResultBudgetMaxTokens,
	}
}

// Close releases resources held by helpers.
// Should be called when the agent is being shut down.
func (h *helpers) Close() {
	if h.tokenUpdateThrottle != nil {
		h.tokenUpdateThrottle.Stop()
	}
}
