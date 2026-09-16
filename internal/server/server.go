package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
	hibikenasynq "github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/handler/http"
	"github.com/rtc-agent/server/internal/handler/rpc"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/oauth"
	"github.com/rtc-agent/server/internal/svc"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// Server HTTP + WebSocket 服务器
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
	instanceID         string               // unique ID for distributed scanner lock
	staleScannerCancel context.CancelFunc   // cancels the stale turn scanner goroutine
	metrics            *turnagent.PrometheusMetrics // Prometheus metrics (may be nil)
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
	// 启动 goroutine 指标采集（每 10s 采样 goroutine 数量与状态分布）
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

	// 启动 rtc-queue Worker（分布式 turn 执行）
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

	// 启动 HTTP 服务器
	mux := http.NewServeMux()

	// 注册路由
	s.registerRoutes(mux)

	// 挂载中间件（Chain 模式：第一个最外层，最后一个最接近 handler）
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

// Stop 停止服务器
func (s *Server) Stop() {
	if logger.IsDebugMode() {
		logger.Debug(context.Background(), "[Server] stopping...")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.ShutdownTimeout)
	defer cancel()

	// 停止 rtc-queue Worker
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

	// 关闭 RPC Handler（停止 recorder worker）
	if s.rpcHandler != nil {
		s.rpcHandler.Close()
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

	// Prometheus 指标端点（可选 basic auth）
	metricsHandler := promhttp.Handler()
	if s.cfg.Metrics.User != "" && s.cfg.Metrics.Password != "" {
		metricsHandler = basicAuth(metricsHandler, s.cfg.Metrics.User, s.cfg.Metrics.Password)
	} else if s.cfg.Server.Env != "development" {
		logger.Warn(context.Background(), "metrics endpoint without authentication - configure metrics.user and metrics.password")
	}
	mux.Handle("GET /metrics", metricsHandler)

	// Debug 端点：pprof + goroutine 监控（可选 basic auth）
	if s.cfg.Debug.Enabled {
		s.registerDebugRoutes(mux)
	}

	// OAuth2 端点
	s.oauth2Handler.RegisterRoutes(mux)

	// Interrupt 端点（前端提交 interrupt 答案）
	isDevInterrupt := s.cfg.Server.Env == "development"
	s.interruptHandler.RegisterRoutes(mux, isDevInterrupt)

	// Memories 端点（Memory 导出）
	isDevMemories := s.cfg.Server.Env == "development"
	s.memoriesHandler.RegisterRoutes(mux, isDevMemories)

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

// registerDebugRoutes 注册 /debug/* 路由（pprof + goroutines）。
// 生产环境应配置 debug.user/password 启用 basic auth 保护。
func (s *Server) registerDebugRoutes(mux *http.ServeMux) {
	hasAuth := s.cfg.Debug.User != "" && s.cfg.Debug.Password != ""

	// goroutines 端点
	goroutinesHandler := middleware.GoroutinesHandler()
	if hasAuth {
		mux.Handle("GET /debug/goroutines", middleware.BasicAuth(goroutinesHandler, s.cfg.Debug.User, s.cfg.Debug.Password))
	} else {
		mux.Handle("GET /debug/goroutines", goroutinesHandler)
	}

	// pprof 端点
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

// NewWithDeps 创建服务器（Wire 兼容版本）。
// 所有依赖由调用方提供，便于 Wire 注入。
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

// resumeWorkPayload 是 recoverStaleTurns 发布的 resume 工作项 JSON 载体。
// 使用 struct + json.Marshal 替代 fmt.Sprintf 拼接 JSON，
// 避免字符串转义风险并保证字段类型安全。
type resumeWorkPayload struct {
	Kind        string `json:"kind"`
	SessionID   string `json:"session_id"`
	InterruptID string `json:"interrupt_id"`
}

// recoverStaleTurns runs at startup to recover all stale turns left behind
// after a crash. It finds every turn in running/pending/interrupted state,
// marks them as interrupted, and publishes a resume work item so the agent
// can continue. Finally, it requeues ghost work items and releases stale
// session locks.
//
// Differences from periodicRecoverStaleTurns (runtime scanner):
//   - No time thresholds — recovers ALL stale turns immediately.
//   - No Worker liveness or checkpoint checks (no Workers connected at startup).
//   - Publishes kind="resume" (preserving InterruptID) instead of kind="submit".
//   - Performs ghost-work cleanup and session-lock release (runtime scanner does not).
func (s *Server) recoverStaleTurns(ctx context.Context) {
	staleTurns, err := s.svcCtx.TurnRepo.FindStaleTurns(ctx, staleTurnStatuses)
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
		if turn.Status != string(model.TurnStatusInterrupted) {
			if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, model.TurnStatusInterrupted, "server restart recovery"); err != nil {
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
			payloadBytes, err := json.Marshal(&resumeWorkPayload{
				Kind:        "resume",
				SessionID:   turn.SessionID.String(),
				InterruptID: interruptID,
			})
			if err != nil {
				logger.Error(ctx, "[Server] recoverStaleTurns: marshal resume payload",
					zap.String("turn_id", turn.ID.String()),
					zap.Error(err))
				continue
			}
			payload := string(payloadBytes)

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

// staleTurnScanner runs periodically to recover stale turns that got stuck
// during runtime (e.g., worker crash, network partition). Uses a Redis
// distributed lock to ensure only one Server instance scans at a time.
func (s *Server) staleTurnScanner(ctx context.Context) {
	const (
		scannerInterval = 5 * time.Minute
		scannerLockKey  = "stale_turn_scanner_lock"
		scannerLockTTL  = 4 * time.Minute // < 5min interval
	)

	ticker := time.NewTicker(scannerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Redis distributed lock: only one instance scans at a time.
			acquired, err := s.svcCtx.Redis.SetNX(ctx, scannerLockKey,
				s.instanceID, scannerLockTTL).Result()
			if err != nil {
				logger.Warn(ctx, "[Server] staleTurnScanner: lock acquisition failed",
					zap.Error(err))
				continue // Redis failure: skip this round
			}
			if !acquired {
				if logger.IsDebugMode() {
					logger.Debug(ctx, "[Server] staleTurnScanner: lock held by another instance")
				}
				continue
			}

			s.periodicRecoverStaleTurns(ctx)
		}
	}
}

// periodicRecoverStaleTurns scans for stale turns and recovers them based on
// time thresholds and Worker liveness. Called by the periodic staleTurnScanner.
//
// Differences from recoverStaleTurns (startup recovery):
//   - Applies time thresholds (running > 10min, pending > 2min, interrupted > 30min).
//   - Checks Worker liveness (session lock TTL) and checkpoint existence before recovery.
//   - Per-status transitions: running → interrupted/failed, pending → failed, interrupted → cancelled.
//   - Publishes kind="submit" via publishRecoveryWorkItem (not "resume").
//   - Records Prometheus metrics and syncs session status after recovery.
//   - Uses LIMIT 100 (startup uses no limit).
func (s *Server) periodicRecoverStaleTurns(ctx context.Context) {
	const (
		runningThreshold     = 10 * time.Minute
		pendingThreshold     = 2 * time.Minute
		interruptedThreshold = 30 * time.Minute
		scanLimit            = 100
	)

	staleTurns, err := s.svcCtx.TurnRepo.FindStaleTurnsWithLimit(ctx, staleTurnStatuses, scanLimit)
	if err != nil {
		logger.Error(ctx, "[Server] periodicRecoverStaleTurns: find stale turns", zap.Error(err))
		return
	}

	if len(staleTurns) == 0 {
		return
	}

	logger.Info(ctx, "[Server] periodicRecoverStaleTurns: found stale turns",
		zap.Int("count", len(staleTurns)))

	now := time.Now()

	for _, turn := range staleTurns {
		sessionID := turn.SessionID.String()

		// Determine age based on status:
		// - running: use StartedAt (when it began executing)
		// - pending/interrupted: use CreatedAt
		var age time.Duration
		switch turn.Status {
		case string(model.TurnStatusRunning):
			if turn.StartedAt != nil {
				age = now.Sub(*turn.StartedAt)
			} else {
				age = now.Sub(turn.CreatedAt)
			}
			if age < runningThreshold {
				continue
			}
			// Check if a Worker is still alive (holding the session lock).
			// If so, the turn may still be progressing normally.
			if s.isWorkerAliveForSession(ctx, sessionID) {
				continue
			}
			// Check if checkpoint still exists. If expired, resume will fail,
			// so mark as failed directly.
			checkpointKey := cache.Checkpoint("session:" + sessionID)
			exists, err := s.svcCtx.Redis.Exists(ctx, checkpointKey).Result()
			if err != nil || exists == 0 {
				logger.Warn(ctx, "[Server] periodicRecoverStaleTurns: checkpoint expired",
					zap.String("turn_id", turn.ID.String()),
					zap.String("session_id", sessionID))
				if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, model.TurnStatusFailed, "periodic scanner: checkpoint expired"); err != nil {
					logger.Error(ctx, "[Server] periodicRecoverStaleTurns: update failed",
						zap.String("turn_id", turn.ID.String()), zap.Error(err))
				} else {
					s.syncSessionStatusAfterRecovery(ctx, turn)
					s.recordStaleTurnRecovery("running")
				}
				continue
			}
			logger.Warn(ctx, "[Server] periodicRecoverStaleTurns: recovering stale running turn",
				zap.String("turn_id", turn.ID.String()),
				zap.Duration("age", age))
			if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, model.TurnStatusInterrupted, "periodic scanner: stale running turn"); err != nil {
				logger.Error(ctx, "[Server] periodicRecoverStaleTurns: update interrupted",
					zap.String("turn_id", turn.ID.String()), zap.Error(err))
				continue
			}
			// Publish recovery work item. If publish fails, the turn is left in
			// interrupted state with no automatic recovery path — sync session
			// status to idle so the frontend is not stuck showing "active".
			if err := s.publishRecoveryWorkItem(ctx, turn); err != nil {
				s.syncSessionStatusAfterRecovery(ctx, turn)
			}
			s.recordStaleTurnRecovery("running")

		case string(model.TurnStatusPending):
			age = now.Sub(turn.CreatedAt)
			if age < pendingThreshold {
				continue
			}
			logger.Warn(ctx, "[Server] periodicRecoverStaleTurns: recovering stale pending turn",
				zap.String("turn_id", turn.ID.String()),
				zap.Duration("age", age))
			if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, model.TurnStatusFailed, "periodic scanner: stale pending turn"); err != nil {
				logger.Error(ctx, "[Server] periodicRecoverStaleTurns: update failed",
					zap.String("turn_id", turn.ID.String()), zap.Error(err))
				continue
			}
			s.syncSessionStatusAfterRecovery(ctx, turn)
			s.recordStaleTurnRecovery("pending")

		case string(model.TurnStatusInterrupted):
			age = now.Sub(turn.CreatedAt)
			if age < interruptedThreshold {
				continue
			}
			logger.Warn(ctx, "[Server] periodicRecoverStaleTurns: recovering stale interrupted turn",
				zap.String("turn_id", turn.ID.String()),
				zap.Duration("age", age))
			if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, model.TurnStatusCancelled, "periodic scanner: stale interrupted turn"); err != nil {
				logger.Error(ctx, "[Server] periodicRecoverStaleTurns: update cancelled",
					zap.String("turn_id", turn.ID.String()), zap.Error(err))
				continue
			}
			s.syncSessionStatusAfterRecovery(ctx, turn)
			s.recordStaleTurnRecovery("interrupted")
		}
	}
}

// isWorkerAliveForSession checks whether a Worker is holding the session lock.
// Uses the rtc-queue lock key format: "session:lock:<sessionID>".
func (s *Server) isWorkerAliveForSession(ctx context.Context, sessionID string) bool {
	lockKey := "session:lock:" + sessionID
	ttl, err := s.svcCtx.Redis.TTL(ctx, lockKey).Result()
	if err != nil {
		return false
	}
	return ttl > 0
}

// publishRecoveryWorkItem publishes a kind="submit" work item to trigger turn
// recovery. Uses submit (not resume) to avoid state race conditions.
// Returns an error if the publish fails so the caller can take fallback action
// (e.g., sync session status to idle).
func (s *Server) publishRecoveryWorkItem(ctx context.Context, turn *model.Turn) error {
	if s.queue == nil {
		return nil
	}
	payload := string(turnagent.MarshalSubmitPayload(turn.SessionID.String(), 0))
	if _, err := s.queue.Publish(ctx, turn.SessionID.String(), payload, 100); err != nil {
		logger.Error(ctx, "[Server] publishRecoveryWorkItem: publish failed",
			zap.String("turn_id", turn.ID.String()),
			zap.Error(err))
		return err
	}
	logger.Info(ctx, "[Server] publishRecoveryWorkItem: published",
		zap.String("turn_id", turn.ID.String()),
		zap.String("session_id", turn.SessionID.String()))
	return nil
}

// syncSessionStatusAfterRecovery updates Session status to idle when the last
// running turn for a session has been recovered. Also publishes a session.updated
// event to the frontend.
func (s *Server) syncSessionStatusAfterRecovery(ctx context.Context, turn *model.Turn) {
	session, err := s.svcCtx.SessionRepo.GetByID(ctx, turn.SessionID)
	if err != nil {
		logger.Warn(ctx, "[Server] syncSessionStatusAfterRecovery: load session failed",
			zap.String("session_id", turn.SessionID.String()), zap.Error(err))
		return
	}
	if session.Status != string(model.SessionStatusActive) {
		return
	}
	// Check if any other running turns exist for this session.
	runningCount, err := s.svcCtx.TurnRepo.CountBySessionAndStatus(ctx, turn.SessionID, string(model.TurnStatusRunning))
	if err != nil {
		logger.Warn(ctx, "[Server] syncSessionStatusAfterRecovery: count running failed",
			zap.String("session_id", turn.SessionID.String()), zap.Error(err))
		return
	}
	if runningCount > 0 {
		return // other turns still running
	}
	if err := s.svcCtx.SessionRepo.UpdateStatus(ctx, turn.SessionID, model.SessionStatusIdle); err != nil {
		logger.Error(ctx, "[Server] syncSessionStatusAfterRecovery: update session failed",
			zap.String("session_id", turn.SessionID.String()), zap.Error(err))
		return
	}
	// Publish session.updated event to frontend.
	s.publishSessionStatusUpdate(ctx, session)
}

// publishSessionStatusUpdate publishes a session.updated event after the
// scanner modifies session status.
func (s *Server) publishSessionStatusUpdate(ctx context.Context, session *model.Session) {
	if s.svcCtx.UpdatePublisher == nil {
		return
	}
	updates := primitives.BuildSessionUpdatedUpdates(session)
	if len(updates) == 0 {
		return
	}
	if _, err := s.svcCtx.UpdatePublisher.Publish(ctx, updates...); err != nil {
		logger.Warn(ctx, "[Server] publishSessionStatusUpdate: publish failed",
			zap.String("session_id", session.ID.String()), zap.Error(err))
	}
}

// recordStaleTurnRecovery records a stale turn recovery metric.
func (s *Server) recordStaleTurnRecovery(status string) {
	if s.metrics == nil {
		return
	}
	s.metrics.RecordStaleTurnRecovery(context.Background(), turnagent.StaleTurnRecoveryAttrs{
		Status: status,
	})
}

// basicAuth HTTP Basic Authentication 中间件。
// 用于保护 /metrics 等内部管理端点。
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
