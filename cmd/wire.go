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
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/agent"
	"github.com/rtc-agent/server/internal/agent/command"
	httphandler "github.com/rtc-agent/server/internal/handler/http"
	rpchandler "github.com/rtc-agent/server/internal/handler/rpc"
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
	"github.com/rtc-agent/server/pkg/circuitbreaker"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/proxy"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"github.com/rtc-agent/server/pkg/webfetch"
	"github.com/rtc-agent/server/pkg/websearch"
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
	provideWebSearchManager,
	provideWebFetchManager,
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
		LogLevel:   centrifuge.LogLevelDebug, // elevated to Debug level
		LogHandler: newCentrifugeLogHandler(),
	})
}

// centrifugeLogHandler forwards Centrifuge logs to the zap logger.
type centrifugeLogHandler struct{}

func newCentrifugeLogHandler() centrifuge.LogHandler {
	return (&centrifugeLogHandler{}).handle
}

func (h *centrifugeLogHandler) handle(entry centrifuge.LogEntry) {
	ctx := context.Background()
	fields := make([]zap.Field, 0, len(entry.Fields)+1)

	// Add Centrifuge fields.
	for k, v := range entry.Fields {
		fields = append(fields, zap.Any(k, v))
	}

	// Add error info (if present).
	if entry.Error != nil {
		fields = append(fields, zap.Error(entry.Error))
	}

	// Map Centrifuge log level to zap log level.
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
	webSearchManager *websearch.WebSearchManager,
	webFetchManager *webfetch.WebFetchManager,
) *usecase.Dependencies {
	deps := &usecase.Dependencies{
		DB:                svcCtx.DB,
		Redis:             svcCtx.Redis,
		SessionRepo:       svcCtx.SessionRepo,
		MessageRepo:       svcCtx.MessageRepo,
		TurnRepo:          svcCtx.TurnRepo,
		RtcRepo:           svcCtx.RtcRepo,
		GoalRepo:          svcCtx.GoalRepo,
		LoopRepo:          svcCtx.LoopRepo,
		UpdatePublisher:   svcCtx.UpdatePublisher,
		ChatModel:         chatModelResult.model,
		LLMConfig:         cfg.LLM,
		SystemPrompt:      cfg.Worker.SystemPrompt,
		WorkerConfig:      cfg.Worker,
		CommandRegistry:   command.NewCommandRegistry(),
		TaskScheduler:     taskScheduler,
		WebSearchManager:  webSearchManager,
		WebFetchManager:   webFetchManager,
	}

	// Inject LLM extractor into WebFetchManager now that deps is available.
	// TokenCallbackHandler is set inside agent.New(), so we pass deps for lazy access.
	if webFetchManager != nil && chatModelResult != nil && chatModelResult.model != nil {
		llmAdapter := server.NewEinoLLMClientAdapter(chatModelResult.model, cfg.LLM, deps)
		if llmAdapter != nil {
			extractor := agent.NewWebFetchLLMExtractor(llmAdapter, nil, cfg.WebFetch.LLMMaxTokens, cfg.WebFetch.MaxLLMExtractPerSession)
			webFetchManager.SetLLMExtractor(extractor)
			logger.Info(context.Background(), "LLM extractor injected into web fetch manager")
		}
	}

	return deps
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
		Deps:                            deps,
		Redis:                           redisClient,
		Queue:                           queue,
		ContextTokensLimit:              cfg.Worker.ContextTokensLimit,
		AutoCompactBufferTokens:         cfg.Worker.AutoCompactBufferTokens,
		CacheHitRateWarnThreshold:       cfg.Worker.CacheHitRateWarnThreshold,
		MaxOutputTokensForSummary:       cfg.Worker.MaxOutputTokensForSummary,
		EnableLLMLogging:                logger.IsDebugMode(),
		CheckpointTTL:                   cfg.Worker.CheckpointTTL,
		StreamChunkTTL:                  cfg.Worker.StreamChunkTTL,
		Logger:                          agent.NewLogger(),
		Tracer:                          otel.GetTracerProvider().Tracer("turnagent"),
		Metrics:                         metrics,
		ModelPricing:                    convertModelPricing(cfg.LLM.Pricing),
		EnableStrategicCacheBreakpoints: cfg.Worker.EnableStrategicCacheBreakpoints,
		ShowRawErrors:                   cfg.Debug.Enabled && cfg.Debug.ShowRawErrors,
	})
}

// convertModelPricing converts the config-layer ModelPricingConfig to the agent-layer ModelPricingConfig.
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

// provideMetrics creates a Prometheus metrics collector.
func provideMetrics() *turnagent.PrometheusMetrics {
	return turnagent.NewPrometheusMetrics()
}

// provideWebSearchManager creates the WebSearchManager if web search is enabled.
// Returns nil when disabled, making the dependency optional.
func provideWebSearchManager(cfg *config.Config) *websearch.WebSearchManager {
	if !cfg.WebSearch.Enabled {
		return nil
	}

	// Build search providers from config.
	var providers []websearch.WebSearchProvider
	for _, p := range cfg.WebSearch.Providers {
		if !p.Enabled {
			continue
		}

		var provider websearch.WebSearchProvider
		var err error

		switch p.Type {
		case "duckduckgo":
			ddgCfg := websearch.DefaultDuckDuckGoConfig()
			if p.Region != "" {
				ddgCfg.Region = p.Region
			}
			if p.MaxResults > 0 {
				ddgCfg.MaxResults = p.MaxResults
			}
			if p.Timeout > 0 {
				ddgCfg.Timeout = p.Timeout
			}
			provider, err = websearch.NewDuckDuckGoProvider(ddgCfg)

		case "bing":
			bingCfg := websearch.DefaultBingConfig()
			if p.APIKey != "" {
				bingCfg.APIKey = p.APIKey
			}
			if p.Endpoint != "" {
				bingCfg.Endpoint = p.Endpoint
			}
			if p.Market != "" {
				bingCfg.Market = p.Market
			}
			if p.MaxResults > 0 {
				bingCfg.MaxResults = p.MaxResults
			}
			if p.Timeout > 0 {
				bingCfg.Timeout = p.Timeout
			}
			provider, err = websearch.NewBingProvider(bingCfg)

		case "searxng":
			searCfg := websearch.DefaultSearXNGConfig()
			if p.BaseURL != "" {
				searCfg.BaseURL = p.BaseURL
			}
			if p.Language != "" {
				searCfg.Language = p.Language
			}
			if p.MaxResults > 0 {
				searCfg.MaxResults = p.MaxResults
			}
			if p.Timeout > 0 {
				searCfg.Timeout = p.Timeout
			}
			provider, err = websearch.NewSearXNGProvider(searCfg)

		case "tavily":
			tavilyCfg := websearch.DefaultTavilyConfig()
			if p.APIKey != "" {
				tavilyCfg.APIKey = p.APIKey
			}
			if p.SearchDepth != "" {
				tavilyCfg.SearchDepth = p.SearchDepth
			}
			if p.MaxResults > 0 {
				tavilyCfg.MaxResults = p.MaxResults
			}
			if p.Timeout > 0 {
				tavilyCfg.Timeout = p.Timeout
			}
			tavilyCfg.IncludeAnswer = p.IncludeAnswer
			provider, err = websearch.NewTavilyProvider(tavilyCfg)

		default:
			logger.Error(context.Background(), "unknown web search provider type", zap.String("type", p.Type))
			continue
		}

		if err != nil {
			logger.Error(context.Background(), "failed to create web search provider",
				zap.String("name", p.Name), zap.Error(err))
			continue
		}
		providers = append(providers, provider)
	}

	if len(providers) == 0 {
		logger.Warn(context.Background(), "web search enabled but no valid providers configured, web search will be disabled")
		return nil
	}

	// Convert config types.
	wsCfg := websearch.WebSearchConfig{
		BalancerType:    cfg.WebSearch.BalancerType,
		GlobalTimeout:   cfg.WebSearch.GlobalTimeout,
		ProxyHealthURL:  cfg.WebSearch.ProxyHealthURL,
		ProxyCheckInterval: cfg.WebSearch.ProxyCheckInterval,
		CircuitBreaker: circuitbreaker.CircuitBreakerConfig{
			FailureThreshold:    cfg.WebSearch.CircuitBreaker.FailureThreshold,
			OpenTimeout:         cfg.WebSearch.CircuitBreaker.OpenTimeout,
			HalfOpenMaxRequests: cfg.WebSearch.CircuitBreaker.HalfOpenMaxRequests,
			WindowSize:          cfg.WebSearch.CircuitBreaker.WindowSize,
			WindowDuration:      cfg.WebSearch.CircuitBreaker.WindowDuration,
		},
		RateLimiter: websearch.RateLimiterConfig{
			Rate:  cfg.WebSearch.RateLimiter.Rate,
			Burst: cfg.WebSearch.RateLimiter.Burst,
		},
		Retry: websearch.RetryConfig{
			MaxRetries:    cfg.WebSearch.Retry.MaxRetries,
			RetryDelay:    cfg.WebSearch.Retry.RetryDelay,
			BackoffFactor: cfg.WebSearch.Retry.BackoffFactor,
		},
	}

	// Convert provider configs.
	for _, p := range cfg.WebSearch.Providers {
		if !p.Enabled {
			continue
		}
		wsCfg.Providers = append(wsCfg.Providers, websearch.ProviderConfig{
			Name:    p.Name,
			Type:    p.Type,
			Weight:  p.Weight,
			Enabled: p.Enabled,
		})
	}

	// Convert proxy configs.
	for _, p := range cfg.WebSearch.Proxies {
		wsCfg.Proxies = append(wsCfg.Proxies, proxy.ProxyConfig{
			URL:      p.URL,
			Type:     proxy.ProxyType(p.Type),
			Region:   p.Region,
			Priority: p.Priority,
		})
	}

	manager, err := websearch.NewWebSearchManager(wsCfg, providers, nil)
	if err != nil {
		logger.Error(context.Background(), "failed to create web search manager, web search will be disabled", zap.Error(err))
		return nil
	}

	logger.Info(context.Background(), "web search manager initialized",
		zap.Int("providers", len(providers)),
		zap.Int("proxies", len(wsCfg.Proxies)),
	)
	return manager
}

// provideWebFetchManager creates the WebFetchManager if web fetch is enabled.
// Returns nil when disabled, making the dependency optional.
// LLM extractor is injected later in provideUsecaseDependencies (after deps is created).
func provideWebFetchManager(cfg *config.Config, redisClient redis.UniversalClient) *webfetch.WebFetchManager {
	if !cfg.WebFetch.Enabled {
		return nil
	}

	wfCfg := webfetch.WebFetchConfig{
		Enabled:                    cfg.WebFetch.Enabled,
		MaxConcurrency:             cfg.WebFetch.MaxConcurrency,
		MaxDomainConcurrency:       cfg.WebFetch.MaxDomainConcurrency,
		CacheTTL:                   cfg.WebFetch.CacheTTL,
		MaxURLLength:               cfg.WebFetch.MaxURLLength,
		MaxContentSize:             cfg.WebFetch.MaxContentSize,
		FetchTimeout:               cfg.WebFetch.FetchTimeout,
		MaxRedirects:               cfg.WebFetch.MaxRedirects,
		LLMExtractThresholdBytes:   cfg.WebFetch.LLMExtractThresholdBytes,
		LLMMaxTokens:               cfg.WebFetch.LLMMaxTokens,
		MaxLLMExtractPerSession:    cfg.WebFetch.MaxLLMExtractPerSession,
		UserAgent:                  cfg.WebFetch.UserAgent,
		RespectRobotsTxt:           cfg.WebFetch.RespectRobotsTxt,
		RobotsCacheTTL:             cfg.WebFetch.RobotsCacheTTL,
		PreApprovedDomains:         cfg.WebFetch.PreApprovedDomains,
		BlockedDomains:             cfg.WebFetch.BlockedDomains,
		AllowedSchemes:             []string{"https", "http"},
		RateLimit: webfetch.RateLimitConfig{
			GlobalRPS:   cfg.WebFetch.RateLimit.GlobalRPS,
			GlobalBurst: cfg.WebFetch.RateLimit.GlobalBurst,
			DomainRPS:   cfg.WebFetch.RateLimit.DomainRPS,
			DomainBurst: cfg.WebFetch.RateLimit.DomainBurst,
		},
	}

	// Fall back to default pre-approved domains if none configured.
	if len(wfCfg.PreApprovedDomains) == 0 {
		defaults := webfetch.DefaultWebFetchConfig()
		wfCfg.PreApprovedDomains = defaults.PreApprovedDomains
	}

	// Fall back to default rate limit config if not configured.
	if wfCfg.RateLimit.GlobalRPS == 0 {
		wfCfg.RateLimit = webfetch.DefaultRateLimitConfig()
	}

	// Fall back to default robots cache TTL if not configured.
	if wfCfg.RobotsCacheTTL == 0 {
		wfCfg.RobotsCacheTTL = 24 * time.Hour
	}

	// Fall back to default LLM max tokens if not configured.
	if wfCfg.LLMMaxTokens == 0 {
		wfCfg.LLMMaxTokens = 4096
	}

	// NOTE: pass nil for loggers to match websearch pattern (uses zap.NewNop).
	// TODO: export zap logger from pkg/logger for production audit logging.
	manager, err := webfetch.NewWebFetchManager(wfCfg, redisClient, nil, nil)
	if err != nil {
		logger.Error(context.Background(), "failed to create web fetch manager, web fetch will be disabled", zap.Error(err))
		return nil
	}

	if err := manager.Start(context.Background()); err != nil {
		logger.Error(context.Background(), "failed to start web fetch manager", zap.Error(err))
		return nil
	}

	logger.Info(context.Background(), "web fetch manager initialized")
	return manager
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

func (l *workerLogger) Info(ctx context.Context, msg string, keysAndValues ...any) {
	logger.Info(ctx, "[rtcqueue] "+msg, toZapFields(keysAndValues)...)
}

func (l *workerLogger) Error(ctx context.Context, msg string, keysAndValues ...any) {
	logger.Error(ctx, "[rtcqueue] "+msg, toZapFields(keysAndValues)...)
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
	metrics *turnagent.PrometheusMetrics,
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
		metrics,
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
func provideAsynqMux(queue *rtcqueue.Queue, loopRepo repo.LoopRepo, deps *usecase.Dependencies) *hibikenasynq.ServeMux {
	// Create the notification creator callback for loop worker
	notificationCreator := agent.CreateLoopNotification(deps)
	worker := loop.NewWorker(queue, loopRepo, notificationCreator)
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
