//go:build wireinject
// +build wireinject

package cmd

import (
	"context"
	"time"

	"github.com/google/uuid"
	hibikenasynq "github.com/hibiken/asynq"
	centrifugeplus "github.com/rtc-agent/server/pkg/centrifuge-plus"

	"github.com/centrifugal/centrifuge"
	"github.com/cloudwego/eino/components/model"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/agent"
	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/handler/http"
	"github.com/rtc-agent/server/internal/handler/rpc"
	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/loop"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/server"
	"github.com/rtc-agent/server/internal/svc"
	"github.com/rtc-agent/server/internal/taskscheduler"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// =============================================================================
// Wire provider sets
// =============================================================================

// RepositorySet provides all repository implementations.
var RepositorySet = wire.NewSet(
	repo.NewSessionRepo,
	repo.NewMessageRepo,
	repo.NewTurnRepo,
	repo.NewRtcRepo,
	repo.NewGoalRepo,
	repo.NewOAuth2UserRepo,
	repo.NewDeviceRepo,
	repo.NewRefreshTokenRepo,
	repo.NewSessionMemoryRepo,
	repo.NewUserMemoryRepo,
	repo.NewScriptExecutionRepo,
	repo.NewMemoryRepo,
	repo.NewLoopRepo,
)

// ServiceSet provides core services (UpdatePublisher, JWTSigner, Centrifuge).
var ServiceSet = wire.NewSet(
	provideRedisUniversal,
	provideUpdatePublisher,
	provideJWTSigner,
	provideCentrifugeNode,
	provideDualBroker,
)

// UsecaseSet provides usecase layer dependencies.
var UsecaseSet = wire.NewSet(
	provideMetrics,
	provideChatModel,
	provideTaskScheduler,
	provideUsecaseDependencies,
)

// AsynqSet provides asynq components for loop task scheduling.
var AsynqSet = wire.NewSet(
	provideAsynqServer,
	provideAsynqMux,
	provideAsynqInspector,
	provideRecoveryCancel,
)

// QueueSet provides rtc-queue components.
var QueueSet = wire.NewSet(
	provideQueue,
	provideStreamStore,
	provideAgent,
	provideQueueWorker,
)

// HandlerSet provides all HTTP and RPC handlers.
var HandlerSet = wire.NewSet(
	provideStateStore,
	provideOAuth2ProviderClient,
	provideRPCHandler,
	provideHTTPHandler,
	provideOAuth2Handler,
	provideInterruptHandler,
	provideMemoriesHandler,
)

// ServerSet provides the main Server.
var ServerSet = wire.NewSet(
	provideServer,
)

// =============================================================================
// Provider functions
// =============================================================================

func provideRedisUniversal(rdb *redis.Client) redis.UniversalClient {
	return rdb
}

func provideUpdatePublisher(
	db *gorm.DB,
	redisClient redis.UniversalClient,
	sessionRepo repo.SessionRepo,
	messageRepo repo.MessageRepo,
	turnRepo repo.TurnRepo,
	rtcRepo repo.RtcRepo,
) *updates.UpdatePublisher {
	return updates.NewUpdatePublisher(db, redisClient, sessionRepo, messageRepo, turnRepo, rtcRepo)
}

func provideJWTSigner(cfg *config.Config) (*auth.JWTSigner, error) {
	return auth.NewJWTSigner(cfg.Auth.JWTSecret, time.Duration(cfg.Auth.AccessTokenTTLSeconds)*time.Second)
}

func provideCentrifugeNode() (*centrifuge.Node, error) {
	return centrifuge.New(centrifuge.Config{
		LogLevel:   centrifuge.LogLevelDebug, // 提高到 Debug 级别
		LogHandler: newCentrifugeLogHandler(),
	})
}

// centrifugeLogHandler 将 Centrifuge 的日志转发到 zap logger
type centrifugeLogHandler struct{}

func newCentrifugeLogHandler() centrifuge.LogHandler {
	return (&centrifugeLogHandler{}).handle
}

func (h *centrifugeLogHandler) handle(entry centrifuge.LogEntry) {
	ctx := context.Background()
	fields := make([]zap.Field, 0, len(entry.Fields)+1)

	// 添加 Centrifuge 的字段
	for k, v := range entry.Fields {
		fields = append(fields, zap.Any(k, v))
	}

	// 添加错误信息（如果有）
	if entry.Error != nil {
		fields = append(fields, zap.Error(entry.Error))
	}

	// 根据 Centrifuge 的日志级别映射到 zap 的日志级别
	switch entry.Level {
	case centrifuge.LogLevelTrace, centrifuge.LogLevelDebug:
		logger.Debug(ctx, "[centrifuge] "+entry.Message, fields...)
	case centrifuge.LogLevelInfo:
		logger.Info(ctx, "[centrifuge] "+entry.Message, fields...)
	case centrifuge.LogLevelWarn:
		logger.Warn(ctx, "[centrifuge] "+entry.Message, fields...)
	case centrifuge.LogLevelError:
		logger.Error(ctx, "[centrifuge] "+entry.Message, fields...)
	default:
		logger.Info(ctx, "[centrifuge] "+entry.Message, fields...)
	}
}

func provideDualBroker(
	cfg *config.Config,
	node *centrifuge.Node,
	updatePublisher *updates.UpdatePublisher,
	jwtSigner *auth.JWTSigner,
) (*centrifugeplus.DualBroker, error) {
	return svc.AssembleDualBroker(node, cfg, updatePublisher, jwtSigner)
}

// chatModelResult wraps the optional ChatModel to handle Wire's error semantics.
type chatModelResult struct {
	model model.ToolCallingChatModel
}

func provideChatModel(cfg *config.Config, metrics *turnagent.PrometheusMetrics) (*chatModelResult, error) {
	if cfg.LLM.Provider == "" || cfg.LLM.Model == "" {
		logger.Warn(context.Background(), "LLM not configured (provider/model missing), agent features will be disabled")
		return &chatModelResult{model: nil}, nil
	}

	m, err := server.NewChatModel(cfg, metrics)
	if err != nil {
		logger.Error(context.Background(), "Failed to create chat model (agent features will be disabled)", zap.Error(err))
		return &chatModelResult{model: nil}, nil
	}
	logger.Info(context.Background(), "LLM initialized", zap.String("provider", cfg.LLM.Provider), zap.String("model", cfg.LLM.Model))
	return &chatModelResult{model: m}, nil
}

func provideUsecaseDependencies(
	svcCtx *svc.ServiceContext,
	chatModelResult *chatModelResult,
	cfg *config.Config,
	taskScheduler usecase.TaskScheduler,
) *usecase.Dependencies {
	return &usecase.Dependencies{
		DB:                svcCtx.DB,
		Redis:             svcCtx.Redis,
		SessionRepo:       svcCtx.SessionRepo,
		MessageRepo:       svcCtx.MessageRepo,
		TurnRepo:          svcCtx.TurnRepo,
		RtcRepo:           svcCtx.RtcRepo,
		GoalRepo:          svcCtx.GoalRepo,
		LoopRepo:          svcCtx.LoopRepo,
		SessionMemoryRepo: svcCtx.SessionMemoryRepo,
		UserMemoryRepo:    svcCtx.UserMemoryRepo,
		UpdatePublisher:   svcCtx.UpdatePublisher,
		ChatModel:         chatModelResult.model,
		LLMConfig:         cfg.LLM,
		SystemPrompt:      cfg.Worker.SystemPrompt,
		WorkerConfig:      cfg.Worker,
		CommandRegistry:   command.NewCommandRegistry(),
		TaskScheduler:     taskScheduler,
	}
}

func provideQueue(rdb *redis.Client) *rtcqueue.Queue {
	return rtcqueue.New(rdb)
}

func provideStreamStore(redisClient redis.UniversalClient, cfg *config.Config) *agent.StreamStore {
	return agent.NewStreamStore(redisClient, cfg.Worker.StreamChunkTTL)
}

func provideAgent(
	deps *usecase.Dependencies,
	redisClient redis.UniversalClient,
	queue *rtcqueue.Queue,
	cfg *config.Config,
	metrics *turnagent.PrometheusMetrics,
) (*turnagent.Agent, error) {
	return agent.New(agent.Config{
		Deps:                      deps,
		Redis:                     redisClient,
		Queue:                     queue,
		ContextTokensLimit:        cfg.Worker.ContextTokensLimit,
		AutoCompactBufferTokens:   cfg.Worker.AutoCompactBufferTokens,
		CacheHitRateWarnThreshold: cfg.Worker.CacheHitRateWarnThreshold,
		MaxOutputTokensForSummary: cfg.Worker.MaxOutputTokensForSummary,
		EnableLLMLogging:          logger.IsDebugMode(),
		CheckpointTTL:             cfg.Worker.CheckpointTTL,
		StreamChunkTTL:            cfg.Worker.StreamChunkTTL,
		Logger:                    agent.NewLogger(),
		Metrics:                   metrics,
		ModelPricing:              convertModelPricing(cfg.LLM.Pricing),
	})
}

// convertModelPricing 将配置层的 ModelPricingConfig 转换为 agent 层的 ModelPricingConfig
func convertModelPricing(cfg *config.ModelPricingConfig) *agent.ModelPricingConfig {
	if cfg == nil {
		return nil
	}
	return &agent.ModelPricingConfig{
		InputPerMillion:       cfg.InputPerMillion,
		OutputPerMillion:      cfg.OutputPerMillion,
		CachedReadPerMillion:  cfg.CachedReadPerMillion,
		CachedWritePerMillion: cfg.CachedWritePerMillion,
		ReasoningPerMillion:   cfg.ReasoningPerMillion,
	}
}

// provideMetrics 创建 Prometheus 指标收集器
func provideMetrics() *turnagent.PrometheusMetrics {
	return turnagent.NewPrometheusMetrics()
}

func provideQueueWorker(
	queue *rtcqueue.Queue,
	agent *turnagent.Agent,
	cfg *config.Config,
) *rtcqueue.Worker {
	workerID := cfg.Worker.WorkerID
	if workerID == "" {
		workerID = "worker-" + uuid.Must(uuid.NewV7()).String()
	}

	return rtcqueue.NewWorker(queue, rtcqueue.WorkerConfig{
		WorkerID:    workerID,
		Concurrency: cfg.Worker.BackgroundConcurrency,
		OnWork:      agent.Process,
		OnError: func(err error) {
			logger.Error(context.Background(), "[rtcqueue.Worker] error", zap.Error(err))
		},
		Logger:   &workerLogger{},
		HoldLock: true, // Enable hold-lock mode for turn-loop agent
	})
}

// workerLogger adapts the application logger to rtcqueue.WorkerLogger interface.
type workerLogger struct{}

func (l *workerLogger) Info(msg string, keysAndValues ...any) {
	logger.Info(context.Background(), "[rtcqueue] "+msg, toZapFields(keysAndValues)...)
}

func (l *workerLogger) Error(msg string, keysAndValues ...any) {
	logger.Error(context.Background(), "[rtcqueue] "+msg, toZapFields(keysAndValues)...)
}

// toZapFields converts key-value pairs to []zap.Field.
func toZapFields(kv []any) []zap.Field {
	fields := make([]zap.Field, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		fields = append(fields, zap.Any(key, kv[i+1]))
	}
	return fields
}

func provideStateStore(redisClient redis.UniversalClient) *oauth.RedisStore {
	return oauth.NewRedisStore(redisClient)
}

func provideOAuth2ProviderClient(cfg *config.Config) *oauth.Client {
	providers := server.BuildProviderClients(cfg)
	return oauth.NewClient(providers, cfg.Providers.HTTPTimeout)
}

func provideRPCHandler(
	svcCtx *svc.ServiceContext,
	deps *usecase.Dependencies,
	sessionRepo repo.SessionRepo,
	queue *rtcqueue.Queue,
	cfg *config.Config,
	metrics *turnagent.PrometheusMetrics,
	inspector *hibikenasynq.Inspector,
) *rpchandler.Handler {
	handler := rpchandler.NewHandler(&rpchandler.Dependencies{
		Deps:                deps,
		SessionRepo:         sessionRepo,
		Queue:               queue,
		API:                 cfg.API,
		ScriptExecutionRepo: svcCtx.ScriptExecutionRepo,
		Metrics:             metrics,
		AsynqInspector:      inspector,
	})
	// Register globally for Centrifuge RPC callbacks
	svc.RegisterRPCHandler(handler)
	return handler
}

func provideHTTPHandler(svcCtx *svc.ServiceContext) *httphandler.Handler {
	return httphandler.NewHandler(svcCtx)
}

func provideOAuth2Handler(
	svcCtx *svc.ServiceContext,
	jwtSigner *auth.JWTSigner,
	stateStore *oauth.RedisStore,
	providerClient *oauth.Client,
	cfg *config.Config,
) *httphandler.OAuth2Handler {
	return httphandler.NewOAuth2Handler(svcCtx, jwtSigner, stateStore, providerClient, cfg.Auth)
}

func provideInterruptHandler(
	redisClient redis.UniversalClient,
	cfg *config.Config,
	svcCtx *svc.ServiceContext,
	jwtSigner *auth.JWTSigner,
) *httphandler.InterruptHandler {
	return httphandler.NewInterruptHandler(redisClient, cfg.Worker, svcCtx.SessionRepo, jwtSigner)
}

func provideMemoriesHandler(
	svcCtx *svc.ServiceContext,
	jwtSigner *auth.JWTSigner,
) *httphandler.MemoriesHandler {
	return httphandler.NewMemoriesHandler(svcCtx, jwtSigner)
}

func provideServer(
	cfg *config.Config,
	svcCtx *svc.ServiceContext,
	rpcHandler *rpchandler.Handler,
	httpHandler *httphandler.Handler,
	oauth2Handler *httphandler.OAuth2Handler,
	interruptHandler *httphandler.InterruptHandler,
	memoriesHandler *httphandler.MemoriesHandler,
	queueWorker *rtcqueue.Worker,
	queue *rtcqueue.Queue,
	streamStore *agent.StreamStore,
	asynqServer *hibikenasynq.Server,
	asynqMux *hibikenasynq.ServeMux,
	recoveryCancel context.CancelFunc,
) *server.Server {
	// Inject stream store into UpdatePublisher
	svcCtx.UpdatePublisher.SetStreamStore(streamStore)

	return server.NewWithDeps(
		cfg,
		svcCtx,
		rpcHandler,
		httpHandler,
		oauth2Handler,
		interruptHandler,
		memoriesHandler,
		queueWorker,
		queue,
		asynqServer,
		asynqMux,
		recoveryCancel,
	)
}

// =============================================================================
// Asynq provider functions
// =============================================================================

// provideTaskScheduler creates the asynq-based TaskScheduler.
// It returns the usecase.TaskScheduler interface for injection into usecase.Dependencies.
func provideTaskScheduler(cfg *config.Config) (usecase.TaskScheduler, error) {
	addr := cfg.Asynq.RedisAddr
	if addr == "" {
		addr = cfg.Redis.Addr
	}
	return taskscheduler.NewTaskScheduler(addr)
}

// provideAsynqServer creates the asynq Server for processing loop tasks.
func provideAsynqServer(cfg *config.Config) *hibikenasynq.Server {
	addr := cfg.Asynq.RedisAddr
	if addr == "" {
		addr = cfg.Redis.Addr
	}
	concurrency := cfg.Asynq.Concurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	queueName := cfg.Asynq.Queue
	if queueName == "" {
		queueName = "loop"
	}
	return hibikenasynq.NewServer(
		hibikenasynq.RedisClientOpt{Addr: addr},
		hibikenasynq.Config{
			Concurrency: concurrency,
			Queues: map[string]int{
				queueName: 6,
				"default": 3,
			},
		},
	)
}

// provideAsynqMux creates the asynq ServeMux with loop task handlers registered.
func provideAsynqMux(queue *rtcqueue.Queue, loopRepo repo.LoopRepo) *hibikenasynq.ServeMux {
	worker := loop.NewWorker(queue, loopRepo)
	mux := hibikenasynq.NewServeMux()
	worker.RegisterHandlers(mux)
	return mux
}

// provideAsynqInspector creates the asynq Inspector for task management.
func provideAsynqInspector(cfg *config.Config) *hibikenasynq.Inspector {
	addr := cfg.Asynq.RedisAddr
	if addr == "" {
		addr = cfg.Redis.Addr
	}
	return hibikenasynq.NewInspector(hibikenasynq.RedisClientOpt{Addr: addr})
}

// provideRecoveryCancel creates the recovery goroutine and returns its cancel function.
func provideRecoveryCancel(
	cfg *config.Config,
	loopRepo repo.LoopRepo,
	scheduler usecase.TaskScheduler,
) context.CancelFunc {
	// Recovery needs direct access to asynq client/inspector.
	// Extract them from the concrete scheduler type.
	type clientProvider interface {
		Client() *hibikenasynq.Client
		Inspector() *hibikenasynq.Inspector
	}
	cp, ok := scheduler.(clientProvider)
	if !ok || cp == nil {
		// TaskScheduler not available or wrong type; return no-op cancel.
		return func() {}
	}

	interval := cfg.Asynq.RecoveryInterval
	if interval <= 0 {
		interval = 1 * time.Minute
	}

	staleThreshold := cfg.Asynq.StaleThreshold
	if staleThreshold <= 0 {
		staleThreshold = 5 * time.Minute
	}

	retryMax := cfg.Asynq.RetryMax
	if retryMax < 0 {
		retryMax = 3
	}

	ctx, cancel := context.WithCancel(context.Background())
	go loop.RunRecovery(ctx, loop.RecoveryDeps{
		LoopRepo:       loopRepo,
		Client:         cp.Client(),
		Inspector:      cp.Inspector(),
		Interval:       interval,
		StaleThreshold: staleThreshold,
		RetryMax:       retryMax,
	})
	return cancel
}

// =============================================================================
// Wire initialization
// =============================================================================

// InitializeServiceContext builds the ServiceContext using Wire.
func InitializeServiceContext(
	cfg *config.Config,
	db *gorm.DB,
	rdb *redis.Client,
) (*svc.ServiceContext, error) {
	wire.Build(
		RepositorySet,
		ServiceSet,
		svc.NewServiceContextWithDeps,
	)
	return nil, nil
}

// InitializeServer builds the entire Server using Wire.
func InitializeServer(
	cfg *config.Config,
	db *gorm.DB,
	rdb *redis.Client,
) (*server.Server, error) {
	wire.Build(
		RepositorySet,
		ServiceSet,
		UsecaseSet,
		QueueSet,
		HandlerSet,
		AsynqSet,
		svc.NewServiceContextWithDeps,
		provideServer,
	)
	return nil, nil
}
