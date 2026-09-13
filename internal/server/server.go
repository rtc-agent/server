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
	queue            *rtcqueue.Queue  // rtc-queue for publishing recovery work items
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
	if cfg.Providers.GitHub.Enabled {
		scope := cfg.Providers.GitHub.Scope
		if scope == "" {
			scope = "read:user user:email"
		}
		list = append(list, &oauth.ProviderConfig{
			Name:         "github",
			AuthURL:      "https://github.com/login/oauth/authorize?scope=" + scope,
			TokenURL:     "https://github.com/login/oauth/access_token",
			UserInfoURL:  "https://api.github.com/user",
			ClientID:     cfg.Providers.GitHub.ClientID,
			ClientSecret: cfg.Providers.GitHub.ClientSecret,
		})
	}
	if cfg.Providers.Google.Enabled {
		scope := cfg.Providers.Google.Scope
		if scope == "" {
			scope = "openid email profile"
		}
		list = append(list, &oauth.ProviderConfig{
			Name:         "google",
			AuthURL:      "https://accounts.google.com/o/oauth2/v2/auth?scope=" + scope,
			TokenURL:     "https://oauth2.googleapis.com/token",
			UserInfoURL:  "https://www.googleapis.com/oauth2/v2/userinfo",
			ClientID:     cfg.Providers.Google.ClientID,
			ClientSecret: cfg.Providers.Google.ClientSecret,
		})
	}
	return list
}

// Start 启动服务器
func (s *Server) Start() error {
	// Recover stale turns from previous crash/restart BEFORE starting worker.
	// This ensures that turns left in running/pending state are marked as
	// interrupted and have resume work items published.
	s.recoverStaleTurns(context.Background())

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
		middleware.HTTPMetrics(),
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
			logger.Info(r.Context(), "CheckOrigin called",
				zap.String("origin", origin),
				zap.String("server_env", s.cfg.Server.Env),
				zap.Strings("allow_origins", s.cfg.CORS.AllowOrigins),
			)

			if s.cfg.Server.Env == "development" && len(s.cfg.CORS.AllowOrigins) == 0 {
				// 仅开发模式回退：允许任意 origin
				logger.Info(r.Context(), "CheckOrigin: development mode, allowing all origins")
				return true
			}
			for _, allowed := range s.cfg.CORS.AllowOrigins {
				if allowed == "*" || strings.EqualFold(allowed, origin) {
					logger.Info(r.Context(), "CheckOrigin: origin allowed",
						zap.String("matched", allowed),
					)
					return true
				}
			}
			logger.Error(r.Context(), "CheckOrigin: origin NOT allowed",
				zap.String("origin", origin),
			)
			return false
		},
	})
	mux.Handle("/connection/websocket", wsHandler)
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
	queue *rtcqueue.Queue,
) *Server {
	return &Server{
		cfg:              cfg,
		svcCtx:           svcCtx,
		rpcHandler:       rpcHandler,
		httpHandler:      httpHandler,
		oauth2Handler:    oauth2Handler,
		interruptHandler: interruptHandler,
		queueWorker:      queueWorker,
		queue:            queue,
	}
}

// recoverStaleTurns finds turns left in non-terminal state from a previous
// crash/restart, marks them as interrupted, and publishes resume work items.
// This ensures that work can continue after server restart.
//
// Recovery steps:
//  1. Find turns in running/pending/interrupted state
//  2. For each stale turn, check if its session has a ghost work item
//     (processing but lock expired) and requeue it
//  3. Mark stale turns as interrupted and publish resume work items
//  4. Release stale session locks
func (s *Server) recoverStaleTurns(ctx context.Context) {
	// Find turns in running, pending, OR interrupted state.
	// - running/pending: server crashed while executing
	// - interrupted: server crashed while waiting for external input (RTC)
	staleStatuses := []string{"running", "pending", "interrupted"}
	staleTurns, err := s.svcCtx.TurnRepo.FindStaleTurns(ctx, staleStatuses)
	if err != nil {
		logger.Error(ctx, "[Server] recoverStaleTurns: find stale turns", zap.Error(err))
		return
	}

	if len(staleTurns) == 0 {
		return
	}

	logger.Info(ctx, "[Server] recoverStaleTurns: found stale turns",
		zap.Int("count", len(staleTurns)))

	// Collect unique session IDs for ghost work cleanup
	sessionIDs := make(map[string]bool)

	for _, turn := range staleTurns {
		sessionID := turn.SessionID.String()
		sessionIDs[sessionID] = true

		// Mark as interrupted (if not already)
		if turn.Status != "interrupted" {
			if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, "interrupted", "server restart recovery"); err != nil {
				logger.Error(ctx, "[Server] recoverStaleTurns: update status",
					zap.String("turn_id", turn.ID.String()),
					zap.Error(err))
				continue
			}
		}

		// Publish resume work item with InterruptID if available
		if s.queue != nil {
			// Build payload with InterruptID for proper ResumeParams construction
			interruptID := turn.InterruptID
			payload := fmt.Sprintf(`{"kind":"resume","session_id":"%s","interrupt_id":"%s"}`,
				turn.SessionID.String(), interruptID)

			if _, err := s.queue.Publish(ctx, turn.SessionID.String(), payload, 100); err != nil {
				logger.Error(ctx, "[Server] recoverStaleTurns: publish resume",
					zap.String("turn_id", turn.ID.String()),
					zap.Error(err))
			} else {
				logger.Info(ctx, "[Server] recoverStaleTurns: published resume",
					zap.String("turn_id", turn.ID.String()),
					zap.String("session_id", turn.SessionID.String()),
					zap.String("interrupt_id", interruptID))
			}
		}
	}

	// Clean up ghost work items and release stale session locks.
	// Ghost work: work items left in "processing" state after worker crash.
	// We detect them by checking if session:active pointer exists but lock expired.
	if s.queue != nil {
		sessionIDList := make([]string, 0, len(sessionIDs))
		for sid := range sessionIDs {
			sessionIDList = append(sessionIDList, sid)
		}

		// Batch requeue ghost works (single pipeline round-trip)
		requeued, err := s.queue.RequeueGhostWorksBatch(ctx, sessionIDList)
		if err != nil {
			logger.Warn(ctx, "[Server] recoverStaleTurns: batch requeue ghost works",
				zap.Error(err))
		} else {
			for sid, workID := range requeued {
				logger.Info(ctx, "[Server] recoverStaleTurns: requeued ghost work",
					zap.String("session_id", sid),
					zap.String("work_id", workID))
			}
		}

		// Release session locks (if still held)
		for _, sessionID := range sessionIDList {
			if err := s.queue.ReleaseSession(ctx, sessionID); err != nil {
				logger.Warn(ctx, "[Server] recoverStaleTurns: release session lock",
					zap.String("session_id", sessionID),
					zap.Error(err))
			} else {
				logger.Info(ctx, "[Server] recoverStaleTurns: released session lock",
					zap.String("session_id", sessionID))
			}
		}
	}
}
