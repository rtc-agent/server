//go:build wireinject
// +build wireinject

package cmd

import (
	"context"
	"time"

	centrifugeplus "github.com/rtc-agent/server/pkg/centrifuge-plus"

	"github.com/centrifugal/centrifuge"
	"github.com/cloudwego/eino/components/model"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/agent"
	"github.com/rtc-agent/server/internal/handler/http"
	"github.com/rtc-agent/server/internal/handler/rpc"
	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/server"
	"github.com/rtc-agent/server/internal/svc"
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
	repo.NewOAuth2UserRepo,
	repo.NewDeviceRepo,
	repo.NewRefreshTokenRepo,
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
	provideChatModel,
	provideUsecaseDependencies,
)

// QueueSet provides rtc-queue components.
var QueueSet = wire.NewSet(
	provideQueue,
	provideStreamStore,
	provideMetrics,
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
		LogLevel: centrifuge.LogLevelInfo,
	})
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
	model model.ChatModel
}

func provideChatModel(cfg *config.Config) (*chatModelResult, error) {
	if cfg.LLM.Provider == "" || cfg.LLM.Model == "" {
		logger.Warn(context.Background(), "LLM not configured (provider/model missing), agent features will be disabled")
		return &chatModelResult{model: nil}, nil
	}

	m, err := server.NewChatModel(cfg)
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
) *usecase.Dependencies {
	return &usecase.Dependencies{
		DB:              svcCtx.DB,
		Redis:           svcCtx.Redis,
		SessionRepo:     svcCtx.SessionRepo,
		MessageRepo:     svcCtx.MessageRepo,
		TurnRepo:        svcCtx.TurnRepo,
		RtcRepo:         svcCtx.RtcRepo,
		UpdatePublisher: svcCtx.UpdatePublisher,
		ChatModel:       chatModelResult.model,
		SystemPrompt:    cfg.Worker.SystemPrompt,
		WorkerConfig:    cfg.Worker,
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
	cfg *config.Config,
	metrics *turnagent.PrometheusMetrics,
) (*turnagent.Agent, error) {
	return agent.New(agent.Config{
		Deps:               deps,
		Redis:              redisClient,
		ContextTokensLimit: cfg.Worker.ContextTokensLimit,
		EnableLLMLogging:   logger.DebugMode,
		CheckpointTTL:      cfg.Worker.CheckpointTTL,
		StreamChunkTTL:     cfg.Worker.StreamChunkTTL,
		Logger:             agent.NewLogger(),
		Metrics:            metrics,
	})
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
		workerID = "worker-" + cfg.Server.Host
	}

	return rtcqueue.NewWorker(queue, rtcqueue.WorkerConfig{
		WorkerID:    workerID,
		Concurrency: cfg.Worker.BackgroundConcurrency,
		OnWork:      agent.Process,
		OnError: func(err error) {
			logger.Error(context.Background(), "[rtcqueue.Worker] error", zap.Error(err))
		},
	})
}

func provideStateStore(redisClient redis.UniversalClient) *oauth.RedisStore {
	return oauth.NewRedisStore(redisClient)
}

func provideOAuth2ProviderClient(cfg *config.Config) *oauth.Client {
	providers := server.BuildProviderClients(cfg)
	return oauth.NewClient(providers, cfg.Providers.HTTPTimeout)
}

func provideRPCHandler(
	deps *usecase.Dependencies,
	sessionRepo repo.SessionRepo,
	queue *rtcqueue.Queue,
	cfg *config.Config,
) *rpchandler.Handler {
	handler := rpchandler.NewHandler(&rpchandler.Dependencies{
		Deps:        deps,
		SessionRepo: sessionRepo,
		Queue:       queue,
		API:         cfg.API,
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
) *httphandler.InterruptHandler {
	return httphandler.NewInterruptHandler(redisClient, cfg.Worker)
}

func provideServer(
	cfg *config.Config,
	svcCtx *svc.ServiceContext,
	rpcHandler *rpchandler.Handler,
	httpHandler *httphandler.Handler,
	oauth2Handler *httphandler.OAuth2Handler,
	interruptHandler *httphandler.InterruptHandler,
	queueWorker *rtcqueue.Worker,
	streamStore *agent.StreamStore,
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
		queueWorker,
	)
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
		svc.NewServiceContextWithDeps,
		provideServer,
	)
	return nil, nil
}
