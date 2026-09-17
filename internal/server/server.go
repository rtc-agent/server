package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
	hibikenasynq "github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	httphandler "github.com/rtc-agent/server/internal/handler/http"
	rpchandler "github.com/rtc-agent/server/internal/handler/rpc"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/svc"
	"github.com/rtc-agent/server/pkg/logger"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// Server HTTP + WebSocket server.
type Server struct {
	cfg              *config.Config
	svcCtx           *svc.ServiceContext
	rpcHandler       *rpchandler.Handler
	httpHandler      *httphandler.Handler
	oauth2Handler    *httphandler.OAuth2Handler
	interruptHandler *httphandler.InterruptHandler
	memoriesHandler  *httphandler.MemoriesHandler
	httpServer       *http.Server
	queueWorker      *rtcqueue.Worker // rtc-queue distributed worker
	queue            *rtcqueue.Queue  // rtc-queue for publishing recovery work items
	workerCancel     context.CancelFunc
	asynqServer      *hibikenasynq.Server // asynq worker for loop tasks
	asynqMux         *hibikenasynq.ServeMux
	recoveryCancel   context.CancelFunc // cancels the recovery goroutine
	goroutineCancel  func()             // cancels the goroutine metrics collector

	// Stale turn scanner
	instanceID         string                       // unique ID for distributed scanner lock
	staleScannerCancel context.CancelFunc           // cancels the stale turn scanner goroutine
	metrics            *turnagent.PrometheusMetrics // Prometheus metrics (may be nil)
}

// BuildProviderClients constructs the Provider list from config.
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

// Start starts the server.
func (s *Server) Start() error {
	// Start goroutine metrics collection (samples goroutine count and state distribution every 10s).
	if s.cfg.Debug.Enabled {
		s.goroutineCancel = middleware.StartGoroutineCollector(s.cfg.Debug.GoroutineLeakThreshold)
	}

	// Recover stale turns from previous crash/restart BEFORE starting worker.
	// This ensures that turns left in running/pending state are marked as
	// interrupted and have resume work items published.
	s.recoverStaleTurns(context.Background())

	// Start periodic stale turn scanner goroutine.
	// Uses a Redis distributed lock to ensure only one Server instance scans
	// at a time (see §13.6.2).
	if s.staleScannerCancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.staleScannerCancel = cancel
		logger.SafeGo("stale-turn-scanner", func() {
			s.staleTurnScanner(ctx)
		})
		if logger.IsDebugMode() {
			logger.Debug(ctx, "[Server] stale turn scanner started",
				zap.String("instance_id", s.instanceID))
		}
	}

	// Start rtc-queue Worker (distributed turn execution).
	if logger.IsDebugMode() {
		logger.Debug(context.Background(), "[Server] starting rtc-queue Worker...")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.workerCancel = cancel
	logger.SafeGo("queue-worker", func() {
		if err := s.queueWorker.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error(ctx, "[Server] rtc-queue Worker exited with error", zap.Error(err))
		}
	})
	if logger.IsDebugMode() {
		logger.Debug(ctx, "[Server] rtc-queue Worker started successfully")
	}

	// Start asynq server (loop worker)
	if s.asynqServer != nil && s.asynqMux != nil {
		asynqSrv := s.asynqServer
		asynqMux := s.asynqMux
		logger.SafeGo("asynq-worker", func() {
			if err := asynqSrv.Run(asynqMux); err != nil {
				logger.Error(context.Background(), "[Server] asynq server exited with error", zap.Error(err))
			}
		})
		if logger.IsDebugMode() {
			logger.Debug(ctx, "[Server] asynq worker started")
		}
	}

	// Start HTTP server.
	mux := http.NewServeMux()

	// Register routes.
	s.registerRoutes(mux)

	// Mount middleware (Chain mode: first is outermost, last is closest to handler).
	isDev := s.cfg.Server.Env == "development"
	rateLimiter := middleware.NewRateLimiter(50, 100) // 50 req/s per user, burst 100
	handler := middleware.Chain(
		middleware.CORS(s.cfg.CORS.AllowOrigins, isDev),
		middleware.SecurityHeaders,
		middleware.HTTPMetrics(),
		rateLimiter.Middleware(),
		middleware.RequestLogger,
	)(mux)

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info(ctx, "HTTP server listening", zap.String("addr", addr))
	return s.httpServer.ListenAndServe()
}

// Stop stops the server.
func (s *Server) Stop() {
	if logger.IsDebugMode() {
		logger.Debug(context.Background(), "[Server] stopping...")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.ShutdownTimeout)
	defer cancel()

	// Stop rtc-queue Worker.
	if logger.IsDebugMode() {
		logger.Debug(ctx, "[Server] stopping rtc-queue Worker...")
	}
	if s.workerCancel != nil {
		s.workerCancel()
	}
	if err := s.queueWorker.Stop(ctx); err != nil {
		logger.Error(ctx, "[Server] rtc-queue Worker stop error", zap.Error(err))
	}
	if logger.IsDebugMode() {
		logger.Debug(ctx, "[Server] rtc-queue Worker stopped")
	}

	// Stop asynq server
	if s.asynqServer != nil {
		s.asynqServer.Stop()
		if logger.IsDebugMode() {
			logger.Debug(ctx, "[Server] asynq worker stopped")
		}
	}

	// Stop recovery goroutine
	if s.recoveryCancel != nil {
		s.recoveryCancel()
	}

	// Stop stale turn scanner goroutine
	if s.staleScannerCancel != nil {
		s.staleScannerCancel()
	}

	// Stop goroutine metrics collector
	if s.goroutineCancel != nil {
		s.goroutineCancel()
	}

	// Close RPC Handler (stops recorder worker).
	if s.rpcHandler != nil {
		s.rpcHandler.Close()
	}

	if err := s.svcCtx.CentrifugeNode.Shutdown(ctx); err != nil {
		logger.Error(ctx, "centrifuge node shutdown failed", zap.Error(err))
	}
	if err := s.svcCtx.Broker.Close(ctx); err != nil {
		logger.Error(ctx, "broker close failed", zap.Error(err))
	}

	// Close HTTP Server.
	if err := s.httpServer.Shutdown(ctx); err != nil {
		logger.Error(ctx, "Server shutdown error", zap.Error(err))
	}
}

// registerRoutes registers all routes.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// Public endpoints (no auth required).
	mux.HandleFunc("GET /healthz", s.httpHandler.Healthz)
	mux.HandleFunc("GET /readyz", s.httpHandler.Readyz)

	// Prometheus metrics endpoint (optional basic auth).
	metricsHandler := promhttp.Handler()
	if s.cfg.Metrics.User != "" && s.cfg.Metrics.Password != "" {
		metricsHandler = basicAuth(metricsHandler, s.cfg.Metrics.User, s.cfg.Metrics.Password)
	} else if s.cfg.Server.Env != "development" {
		logger.Warn(context.Background(), "metrics endpoint without authentication - configure metrics.user and metrics.password")
	}
	mux.Handle("GET /metrics", metricsHandler)

	// Debug endpoints: pprof + goroutine monitoring (optional basic auth).
	if s.cfg.Debug.Enabled {
		s.registerDebugRoutes(mux)
	}

	// OAuth2 endpoints.
	s.oauth2Handler.RegisterRoutes(mux)

	// Interrupt endpoints (frontend submits interrupt answers).
	isDevInterrupt := s.cfg.Server.Env == "development"
	s.interruptHandler.RegisterRoutes(mux, isDevInterrupt)

	// Memories endpoints (Memory export).
	isDevMemories := s.cfg.Server.Env == "development"
	s.memoriesHandler.RegisterRoutes(mux, isDevMemories)

	// Centrifuge WebSocket endpoint.
	wsHandler := centrifuge.NewWebsocketHandler(s.svcCtx.CentrifugeNode, centrifuge.WebsocketConfig{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			logger.Info(r.Context(), "CheckOrigin called",
				zap.String("origin", origin),
				zap.String("server_env", s.cfg.Server.Env),
				zap.Strings("allow_origins", s.cfg.CORS.AllowOrigins),
			)

			if s.cfg.Server.Env == "development" && len(s.cfg.CORS.AllowOrigins) == 0 {
				// Development-only fallback: allow any origin.
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

// registerDebugRoutes registers /debug/* routes (pprof + goroutines).
// In production, configure debug.user/password to enable basic auth protection.
func (s *Server) registerDebugRoutes(mux *http.ServeMux) {
	hasAuth := s.cfg.Debug.User != "" && s.cfg.Debug.Password != ""

	// goroutines endpoint
	goroutinesHandler := middleware.GoroutinesHandler()
	if hasAuth {
		mux.Handle("GET /debug/goroutines", middleware.BasicAuth(goroutinesHandler, s.cfg.Debug.User, s.cfg.Debug.Password))
	} else {
		mux.Handle("GET /debug/goroutines", goroutinesHandler)
	}

	// pprof endpoints
	if hasAuth {
		mux.Handle("/debug/pprof/", middleware.PprofHandler(s.cfg.Debug.User, s.cfg.Debug.Password))
		logger.Info(context.Background(), "[Server] debug endpoints protected with basic auth")
	} else {
		middleware.RegisterPprofRoutes(mux)
		if s.cfg.Server.Env != "development" {
			logger.Warn(context.Background(), "debug endpoints without authentication - configure debug.user and debug.password")
		}
	}

	logger.Info(context.Background(), "[Server] debug endpoints registered",
		zap.Bool("auth_enabled", hasAuth),
		zap.Int("goroutine_leak_threshold", s.cfg.Debug.GoroutineLeakThreshold),
	)
}

// NewWithDeps creates the server (Wire-compatible).
// All dependencies are provided by the caller for Wire injection.
func NewWithDeps(
	cfg *config.Config,
	svcCtx *svc.ServiceContext,
	rpcHandler *rpchandler.Handler,
	httpHandler *httphandler.Handler,
	oauth2Handler *httphandler.OAuth2Handler,
	interruptHandler *httphandler.InterruptHandler,
	memoriesHandler *httphandler.MemoriesHandler,
	queueWorker *rtcqueue.Worker,
	queue *rtcqueue.Queue,
	asynqServer *hibikenasynq.Server,
	asynqMux *hibikenasynq.ServeMux,
	recoveryCancel context.CancelFunc,
	metrics *turnagent.PrometheusMetrics,
) *Server {
	instanceID := "server-" + uuid.Must(uuid.NewV7()).String()
	return &Server{
		cfg:              cfg,
		svcCtx:           svcCtx,
		rpcHandler:       rpcHandler,
		httpHandler:      httpHandler,
		oauth2Handler:    oauth2Handler,
		interruptHandler: interruptHandler,
		memoriesHandler:  memoriesHandler,
		queueWorker:      queueWorker,
		queue:            queue,
		asynqServer:      asynqServer,
		asynqMux:         asynqMux,
		recoveryCancel:   recoveryCancel,
		instanceID:       instanceID,
		metrics:          metrics,
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

// staleTurnStatuses is the set of turn statuses considered "stale" — i.e., the
// turn is not in a terminal state (completed / failed / cancelled) and may be
// stuck. Shared by both startup recovery (recoverStaleTurns) and the periodic
// runtime scanner (periodicRecoverStaleTurns).
var staleTurnStatuses = []string{
	string(model.TurnStatusRunning),
	string(model.TurnStatusPending),
	string(model.TurnStatusInterrupted),
}

// basicAuth is an HTTP Basic Authentication middleware.
// Used to protect internal admin endpoints such as /metrics.
func basicAuth(next http.Handler, user, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
