package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/centrifugal/centrifuge"
	"github.com/cloudwego/eino/components/model"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/agent"
	"github.com/rtc-agent/server/internal/handler/http"
	"github.com/rtc-agent/server/internal/handler/rpc"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/svc"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// Server HTTP + WebSocket 服务器
type Server struct {
	cfg              *config.Config
	svcCtx           *svc.ServiceContext
	rpcHandler       *rpchandler.Handler
	httpHandler      *httphandler.Handler
	oauth2Handler    *httphandler.OAuth2Handler
	interruptHandler *httphandler.InterruptHandler
	httpServer       *http.Server
	queueWorker      *rtcqueue.Worker // rtc-queue distributed worker
	workerCancel     context.CancelFunc
}

// New 创建服务器
func New(cfg *config.Config, svcCtx *svc.ServiceContext) *Server {
	// 构造 Redis state 存储（分布式环境使用 Redis）
	stateStore := oauth.NewRedisStore(svcCtx.Redis)

	// 构造 ProviderClient（注册启用的 provider）
	providers := BuildProviderClients(cfg)
	providerClient := oauth.NewClient(providers, cfg.Providers.HTTPTimeout)

	// 创建 ChatModel（LLM 客户端）
	//
	//nolint:staticcheck // TODO: migrate to ToolCallingChatModel
	var chatModel model.ChatModel
	if cfg.LLM.Provider != "" && cfg.LLM.Model != "" {
		var err error
		chatModel, err = newChatModel(context.Background(), &cfg.LLM)
		if err != nil {
			logger.Error(context.Background(), "Failed to create chat model (agent features will be disabled)", zap.Error(err))
		} else {
			logger.Info(context.Background(), "LLM initialized", zap.String("provider", cfg.LLM.Provider), zap.String("model", cfg.LLM.Model))
		}
	} else {
		logger.Warn(context.Background(), "LLM not configured (provider/model missing), agent features will be disabled")
	}

	// 创建 UseCase 层依赖（业务逻辑，不存放在 ServiceContext 中）
	usecaseDeps := &usecase.Dependencies{
		DB:              svcCtx.DB,
		Redis:           svcCtx.Redis,
		SessionRepo:     svcCtx.SessionRepo,
		MessageRepo:     svcCtx.MessageRepo,
		TurnRepo:        svcCtx.TurnRepo,
		RtcRepo:         svcCtx.RtcRepo,
		UpdatePublisher: svcCtx.UpdatePublisher,
		ChatModel:       chatModel,
		SystemPrompt:    cfg.Worker.SystemPrompt,
		WorkerConfig:    cfg.Worker,
	}
	// 创建 RPC Handler（协议适配层，委托 usecase 处理业务逻辑）
	// WorkerID: 优先从配置读取，否则用主机名生成
	workerID := cfg.Worker.WorkerID
	if workerID == "" {
		workerID = "worker-" + cfg.Server.Host
	}

	// 注入 StreamStore 到 UpdatePublisher（用于读取 streaming 状态消息的 chunks）
	// agent.NewStreamStore 使用与 agent 内部相同的 Redis key 格式（rediskey.MessageStream）
	svcCtx.UpdatePublisher.SetStreamStore(agent.NewStreamStore(svcCtx.Redis, cfg.Worker.StreamChunkTTL))

	// Construct the rtc-queue for publishing/cancelling work items.
	// The turn-agent worker uses this same Queue instance to claim and
	// process work items.
	// svcCtx.Redis is redis.UniversalClient, but rtcqueue.New needs
	// *redis.Client. The concrete client is always *redis.Client (see
	// cmd/serve.go), so the assertion is safe.
	rdb, ok := svcCtx.Redis.(*redis.Client)
	if !ok {
		logger.Error(context.Background(), "rtc-queue requires *redis.Client", zap.String("type", fmt.Sprintf("%T", svcCtx.Redis)))
		return nil
	}
	q := rtcqueue.New(rdb)

	// Create the turn-agent with all callbacks wired to the application's
	// dependencies. The agent is stateless: each Process call handles one
	// work item from rtc-queue.
	turnAgent, err := agent.New(agent.Config{
		Deps:               usecaseDeps,
		Redis:              svcCtx.Redis,
		ContextTokensLimit: cfg.Worker.ContextTokensLimit,
		EnableLLMLogging:   logger.DebugMode,
		CheckpointTTL:      cfg.Worker.CheckpointTTL,
		StreamChunkTTL:     cfg.Worker.StreamChunkTTL,
	})
	if err != nil {
		logger.Error(context.Background(), "Failed to create turn-agent", zap.Error(err))
		return nil
	}

	// Create the rtc-queue Worker. It subscribes to session:new notifications,
	// claims work items, manages session-level locks, and dispatches to the
	// turn-agent's Process method.
	queueWorker := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{
		WorkerID:    workerID,
		Concurrency: cfg.Worker.BackgroundConcurrency,
		OnWork:      turnAgent.Process,
		OnError: func(err error) {
			logger.Error(context.Background(), "[rtcqueue.Worker] error", zap.Error(err))
		},
	})

	rpcHandler := rpchandler.NewHandler(&rpchandler.Dependencies{
		Deps:        usecaseDeps,
		SessionRepo: svcCtx.SessionRepo,
		Queue:       q,
		API:         cfg.API,
	})

	// 注册 RPC 处理器（由已连接客户端的 OnRPC 回调使用）
	svc.RegisterRPCHandler(rpcHandler)

	return &Server{
		cfg:              cfg,
		svcCtx:           svcCtx,
		rpcHandler:       rpcHandler,
		httpHandler:      httphandler.NewHandler(svcCtx),
		oauth2Handler:    httphandler.NewOAuth2Handler(svcCtx, svcCtx.JWTSigner, stateStore, providerClient, cfg.Auth),
		interruptHandler: httphandler.NewInterruptHandler(svcCtx.Redis, cfg.Worker),
		queueWorker:      queueWorker,
	}
}

// BuildProviderClients 根据配置构造 Provider 列表
func BuildProviderClients(cfg *config.Config) []*oauth.ProviderConfig {
	var list []*oauth.ProviderConfig
	if cfg.Providers.Mock.Enabled {
		list = append(list, &oauth.ProviderConfig{
			Name:         "mock",
			AuthURL:      cfg.Providers.Mock.URL + "/oauth2/authorize",
			TokenURL:     cfg.Providers.Mock.URL + "/oauth2/token/exchange",
			ClientID:     cfg.Providers.Mock.ClientID,
			ClientSecret: cfg.Providers.Mock.ClientSecret,
		})
	}
	return list
}

// Start 启动服务器
func (s *Server) Start() error {
	// 启动 rtc-queue Worker（分布式 turn 执行）
	if logger.DebugMode {
		logger.Debug(context.Background(), "[Server] starting rtc-queue Worker...")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.workerCancel = cancel
	go func() {
		if err := s.queueWorker.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error(ctx, "[Server] rtc-queue Worker exited with error", zap.Error(err))
		}
	}()
	if logger.DebugMode {
		logger.Debug(ctx, "[Server] rtc-queue Worker started successfully")
	}

	// 启动 HTTP 服务器
	mux := http.NewServeMux()

	// 注册路由
	s.registerRoutes(mux)

	// 挂载中间件（Chain 模式：第一个最外层，最后一个最接近 handler）
	handler := middleware.Chain(
		middleware.CORS(s.cfg.CORS.AllowOrigins),
		middleware.RequestLogger,
	)(mux)

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	logger.Info(ctx, "HTTP server listening", zap.String("addr", addr))
	return s.httpServer.ListenAndServe()
}

// Stop 停止服务器
func (s *Server) Stop() {
	if logger.DebugMode {
		logger.Debug(context.Background(), "[Server] stopping...")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.ShutdownTimeout)
	defer cancel()

	// 停止 rtc-queue Worker
	if logger.DebugMode {
		logger.Debug(ctx, "[Server] stopping rtc-queue Worker...")
	}
	if s.workerCancel != nil {
		s.workerCancel()
	}
	if err := s.queueWorker.Stop(ctx); err != nil {
		logger.Error(ctx, "[Server] rtc-queue Worker stop error", zap.Error(err))
	}
	if logger.DebugMode {
		logger.Debug(ctx, "[Server] rtc-queue Worker stopped")
	}

	_ = s.svcCtx.CentrifugeNode.Shutdown(ctx)
	_ = s.svcCtx.Broker.Close(ctx)

	// 关闭 HTTP Server
	if err := s.httpServer.Shutdown(ctx); err != nil {
		logger.Error(ctx, "Server shutdown error", zap.Error(err))
	}
}

// registerRoutes 注册所有路由
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// 公开端点（无需鉴权）
	mux.HandleFunc("GET /healthz", s.httpHandler.Healthz)
	mux.HandleFunc("GET /readyz", s.httpHandler.Readyz)

	// OAuth2 端点
	s.oauth2Handler.RegisterRoutes(mux)

	// Interrupt 端点（前端提交 interrupt 答案）
	s.interruptHandler.RegisterRoutes(mux)

	// Centrifuge WebSocket 端点
	wsHandler := centrifuge.NewWebsocketHandler(s.svcCtx.CentrifugeNode, centrifuge.WebsocketConfig{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if len(s.cfg.CORS.AllowOrigins) == 0 {
				// 开发回退：允许任意 origin
				return true
			}
			for _, allowed := range s.cfg.CORS.AllowOrigins {
				if allowed == "*" || allowed == origin {
					return true
				}
			}
			return false
		},
	})
	mux.Handle("/connection/websocket", wsHandler)
}

// GetRPCHandler 返回 RPC handler（供 Centrifuge 注册使用）
func (s *Server) GetRPCHandler() *rpchandler.Handler {
	return s.rpcHandler
}

// NewWithDeps 创建服务器（Wire 兼容版本）。
// 所有依赖由调用方提供，便于 Wire 注入。
func NewWithDeps(
	cfg *config.Config,
	svcCtx *svc.ServiceContext,
	rpcHandler *rpchandler.Handler,
	httpHandler *httphandler.Handler,
	oauth2Handler *httphandler.OAuth2Handler,
	interruptHandler *httphandler.InterruptHandler,
	queueWorker *rtcqueue.Worker,
) *Server {
	return &Server{
		cfg:              cfg,
		svcCtx:           svcCtx,
		rpcHandler:       rpcHandler,
		httpHandler:      httpHandler,
		oauth2Handler:    oauth2Handler,
		interruptHandler: interruptHandler,
		queueWorker:      queueWorker,
	}
}
