package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/centrifugal/centrifuge"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/handler/http"
	"github.com/rtc-agent/server/internal/handler/rpc"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/svc"
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
	logger.SafeGo("queue-worker", func() {
		if err := s.queueWorker.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error(ctx, "[Server] rtc-queue Worker exited with error", zap.Error(err))
		}
	})
	if logger.DebugMode {
		logger.Debug(ctx, "[Server] rtc-queue Worker started successfully")
	}

	// 启动 HTTP 服务器
	mux := http.NewServeMux()

	// 注册路由
	s.registerRoutes(mux)

	// 挂载中间件（Chain 模式：第一个最外层，最后一个最接近 handler）
	isDev := s.cfg.Server.Env == "development"
	handler := middleware.Chain(
		middleware.CORS(s.cfg.CORS.AllowOrigins, isDev),
		middleware.SecurityHeaders,
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
	mux.Handle("GET /metrics", promhttp.Handler()) // Prometheus 指标

	// OAuth2 端点
	s.oauth2Handler.RegisterRoutes(mux)

	// Interrupt 端点（前端提交 interrupt 答案）
	s.interruptHandler.RegisterRoutes(mux)

	// Centrifuge WebSocket 端点
	wsHandler := centrifuge.NewWebsocketHandler(s.svcCtx.CentrifugeNode, centrifuge.WebsocketConfig{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if s.cfg.Server.Env == "development" && len(s.cfg.CORS.AllowOrigins) == 0 {
				// 仅开发模式回退：允许任意 origin
				return true
			}
			for _, allowed := range s.cfg.CORS.AllowOrigins {
				if allowed == "*" || strings.EqualFold(allowed, origin) {
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
