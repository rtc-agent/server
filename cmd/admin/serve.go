// Package admin provides the admin-server cobra command and initialization.
package admin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	httphandler "github.com/rtc-agent/server/internal/handler/http"
	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/email"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
)

// httpTimeouts defines standard timeout values for the admin HTTP server.
// These prevent resource exhaustion from slow or idle connections.
const (
	// httpReadTimeout is the maximum duration for reading the entire request,
	// including the body. 30s balances large payload uploads against Slowloris
	// attacks.
	httpReadTimeout = 30 * time.Second

	// httpWriteTimeout is the maximum duration before timing out writes of the
	// response. 30s is generous for most admin API responses while still
	// preventing indefinite hangs.
	httpWriteTimeout = 30 * time.Second

	// httpIdleTimeout is the maximum duration to keep idle connections alive.
	// 120s allows connection reuse for bursty admin traffic without leaking
	// file descriptors.
	httpIdleTimeout = 120 * time.Second
)

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the admin server",
	Long:  `Start the RTC Agent admin server for admin user management and authentication`,
	Run:   runServe,
}

// bootstrapDegraded tracks whether the server started in degraded mode
// (bootstrap failed). It is read by the readiness probe endpoint.
var bootstrapDegraded atomic.Bool

func runServe(cmd *cobra.Command, args []string) {
	// Load admin config
	cfg, err := config.LoadAdminConfig(adminCfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load admin config: %v\n", err)
		os.Exit(1)
	}

	// Validate config
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid admin config: %v\n", err)
		os.Exit(1)
	}

	// Populate dynamic config registry with yaml defaults so that the admin UI
	// (via ServerConfigUsecase) displays actual yaml values as YamlDefault instead
	// of the hardcoded registry fallbacks. The admin server shares the deployment
	// with the main server and can read its config file from the default path.
	populateDynamicConfigDefaults()

	// Init logger
	logger.Init("info", "")
	defer logger.Sync()

	ctx := context.Background()
	logger.Info(ctx, "admin.starting")

	// Log deployment mode: detect multi-instance configuration via environment
	// variable INSTANCE_COUNT. This helps operators verify that Redis-backed
	// features (rate limiting, login protection, policy sync) are properly
	// configured for the deployment topology.
	instanceCount := parseInstanceCount()
	deploymentMode := "single-instance"
	if instanceCount > 1 {
		deploymentMode = "multi-instance"
	}
	logger.Info(ctx, "admin.deployment_mode",
		zap.String("mode", deploymentMode),
		zap.Int("instance_count", instanceCount),
		zap.String("env", cfg.Server.Env))

	// Init database
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.NewGormLogger(true, 200*time.Millisecond),
	})
	if err != nil {
		logger.Fatal(ctx, "admin.database_connection_failed", zap.Error(err))
	}

	// Init Redis (optional -- JWKS caching, distributed rate limiting, and policy sync require it).
	var rdb *redis.Client
	if cfg.Redis.Addr != "" {
		rdb = redis.NewClient(&redis.Options{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		})
		defer func() {
			if rdb != nil {
				_ = rdb.Close()
			}
		}()
		if err := rdb.Ping(ctx).Err(); err != nil {
			logger.Warn(ctx, "admin.redis_unreachable_continuing_without_cache",
				zap.String("addr", cfg.Redis.Addr), zap.Error(err))
			rdb = nil
		} else {
			logger.Info(ctx, "admin.redis_connected",
				zap.String("addr", cfg.Redis.Addr))
		}
	} else {
		logger.Info(ctx, "admin.redis_not_configured_jwks_caching_disabled")
	}

	// Warn if multi-instance deployment is configured but Redis is unavailable.
	// Without Redis, rate limiting, login protection, OTP store, and policy sync
	// fall back to per-instance memory stores, which are inconsistent across
	// replicas.
	if rdb == nil && instanceCount > 1 {
		logger.Warn(ctx, "admin.multi_instance_without_redis_rate_limiting_and_policy_sync_disabled",
			zap.Int("instance_count", instanceCount))
	}

	// Init ban cache (Redis-backed if available, otherwise disabled)
	if rdb != nil {
		middleware.InitBanCache(rdb, 30*time.Second)
	}

	// Init JWT signer
	jwtSigner, err := auth.NewAdminJWTSigner(auth.AdminJWTConfig{
		Algorithm:      cfg.JWT.Algorithm,
		Issuer:         cfg.JWT.Issuer,
		Audience:       cfg.JWT.Audience,
		PrivateKeyPath: cfg.JWT.PrivateKeyPath,
		PublicKeyPath:  cfg.JWT.PublicKeyPath,
		AccessTTL:      time.Duration(cfg.JWT.AccessTokenTTL) * time.Second,
		RefreshTTL:     time.Duration(cfg.JWT.RefreshTokenTTL) * time.Second,
	})
	if err != nil {
		logger.Fatal(ctx, "admin.jwt_signer_init_failed", zap.Error(err))
	}

	// Init Casbin enforcer
	enforcer, err := auth.NewCasbinEnforcer(db)
	if err != nil {
		logger.Fatal(ctx, "admin.casbin_enforcer_init_failed", zap.Error(err))
	}

	// Init Redis policy watcher for multi-instance sync (P0 #1)
	var policyWatcher *auth.PolicyWatcher
	if rdb != nil {
		instanceID := uuid.New().String()
		policyWatcher, err = auth.NewPolicyWatcher(enforcer, rdb, instanceID)
		if err != nil {
			// Policy watcher initialization failed - this is critical for multi-instance deployments
			// Log as error but continue (degraded mode: single-instance behavior)
			logger.Error(ctx, "admin.policy_watcher_init_failed_multi_instance_sync_disabled",
				zap.String("instance_id", instanceID),
				zap.Error(err))
			// Don't set policyWatcher to nil explicitly, it's already nil from declaration
		} else {
			defer func() { _ = policyWatcher.Close() }()
			logger.Info(ctx, "admin.policy_watcher_started", zap.String("instance_id", instanceID))
		}
	}

	// Init repositories
	adminUserRepo := repo.NewAdminUserRepo(db)
	refreshTokenRepo := repo.NewAdminRefreshTokenRepo(db)
	adminRoleRepo := repo.NewAdminRoleRepo(db)
	adminUserRoleRepo := repo.NewAdminUserRoleRepo(db)
	auditLogRepo := repo.NewAuditLogRepo(db)
	oauth2UserRepo := repo.NewOAuth2UserRepo(db)
	mainRefreshTokenRepo := repo.NewRefreshTokenRepo(db)
	configRepo := repo.NewConfigRepo(db)
	deviceRepo := repo.NewDeviceRepo(db)
	sessionRepo := repo.NewSessionRepo(db)
	messageRepo := repo.NewMessageRepo(db)

	// Bootstrap default roles and policies (idempotent, transactional).
	// If bootstrap fails, the server enters "degraded mode": it starts normally
	// but default roles/permissions may be missing, causing authorization errors
	// for admin operations. The readiness probe will report degraded status so
	// load balancers can avoid routing traffic to this instance.
	if err := usecase.BootstrapAdmin(ctx, db, adminRoleRepo, enforcer); err != nil {
		bootstrapDegraded.Store(true)
		logger.Error(ctx, "admin.bootstrap_failed_degraded_mode_default_roles_may_be_missing",
			zap.Error(err))
	} else {
		logger.Info(ctx, "admin.bootstrap_completed")
	}

	// Init rate limiter — prefer Redis-backed for multi-instance deployments
	var rateLimiter httphandler.RateLimiterInterface
	if rdb != nil {
		rateLimiter = httphandler.NewRedisRateLimiter(rdb, cfg.Security.RateLimitPerMinute)
		logger.Info(ctx, "admin.redis_rate_limiter_enabled")
	} else {
		memLimiter := httphandler.NewRateLimiter(httphandler.RateLimitConfig{
			MaxRequestsPerMinute: cfg.Security.RateLimitPerMinute,
		})
		defer memLimiter.Stop()
		rateLimiter = memLimiter
		logger.Info(ctx, "admin.memory_rate_limiter_enabled_single_instance_only")
	}

	// Init login protection — prefer Redis-backed for multi-instance deployments
	var loginProtection usecase.LoginProtectionInterface
	lpCfg := usecase.LoginProtectionConfig{
		MaxAttempts:  cfg.Security.LoginMaxAttempts,
		LockDuration: time.Duration(cfg.Security.LoginLockDuration) * time.Second,
	}
	if rdb != nil {
		loginProtection = usecase.NewRedisLoginProtection(rdb, lpCfg)
		logger.Info(ctx, "admin.redis_login_protection_enabled")
	} else {
		loginProtection = usecase.NewLoginProtection(lpCfg)
		logger.Info(ctx, "admin.memory_login_protection_enabled_single_instance_only")
	}

	// Init email sender (optional — required for email OTP login)
	var emailSender email.Sender
	if cfg.Email.SMTPHost != "" && cfg.Email.FromAddress != "" {
		emailSender = email.NewSMTPSender(email.SMTPConfig{
			Host:        cfg.Email.SMTPHost,
			Port:        cfg.Email.SMTPPort,
			User:        cfg.Email.SMTPUser,
			Password:    cfg.Email.SMTPPassword,
			FromAddress: cfg.Email.FromAddress,
			FromName:    cfg.Email.FromName,
		})
		logger.Info(ctx, "admin.smtp_email_sender_configured",
			zap.String("host", cfg.Email.SMTPHost),
			zap.String("from", cfg.Email.FromAddress))
	} else {
		logger.Warn(ctx, "admin.email_not_configured_otp_login_disabled")
	}

	// Init OTP usecase (requires email sender)
	var emailOTPUsecase *usecase.EmailOTPUsecase
	if emailSender != nil {
		otpCfg := usecase.EmailOTPConfig{
			TTL:               time.Duration(cfg.OTP.TTL) * time.Second,
			Length:            cfg.OTP.Length,
			SendCooldown:      time.Duration(cfg.OTP.SendCooldown) * time.Second,
			MaxSendPerIP:      cfg.OTP.MaxSendPerIP,
			MaxVerifyAttempts: cfg.OTP.MaxVerifyAttempts,
			LockDuration:      time.Duration(cfg.OTP.LockDuration) * time.Second,
		}
		var otpStore usecase.OTPStore
		if rdb != nil {
			otpStore = usecase.NewRedisOTPStore(rdb, cfg.OTP.MaxSendPerIP, cfg.OTP.MaxVerifyAttempts,
				time.Duration(cfg.OTP.LockDuration)*time.Second,
				time.Duration(cfg.OTP.SendCooldown)*time.Second)
			logger.Info(ctx, "admin.redis_otp_store_enabled")
		} else {
			otpStore = usecase.NewMemoryOTPStore(cfg.OTP.MaxSendPerIP, cfg.OTP.MaxVerifyAttempts,
				time.Duration(cfg.OTP.LockDuration)*time.Second,
				time.Duration(cfg.OTP.SendCooldown)*time.Second)
			logger.Info(ctx, "admin.memory_otp_store_enabled_single_instance_only")
		}
		emailOTPUsecase = usecase.NewEmailOTPUsecase(adminUserRepo, emailSender, otpStore, otpCfg)
		logger.Info(ctx, "admin.email_otp_usecase_enabled")
	}

	// Init usecases
	adminAuthUsecase := usecase.NewAdminAuthUsecase(adminUserRepo, refreshTokenRepo, jwtSigner)
	adminAuthUsecase.SetLoginProtection(loginProtection)
	adminRoleUsecase := usecase.NewAdminRoleUsecase(adminRoleRepo, adminUserRoleRepo, auditLogRepo, enforcer)
	permissionUsecase := usecase.NewPermissionUsecase(adminRoleRepo, enforcer, auditLogRepo)
	adminUserRoleUsecase := usecase.NewAdminUserRoleUsecase(adminUserRepo, adminRoleRepo, adminUserRoleRepo, enforcer, auditLogRepo)
	adminUserUsecase := usecase.NewAdminUserUsecase(adminUserRepo, adminUserRoleRepo, adminRoleRepo, enforcer)
	rtcUserUsecase := usecase.NewRtcUserUsecase(db, oauth2UserRepo, mainRefreshTokenRepo, auditLogRepo, deviceRepo)
	rtcSessionUsecase := usecase.NewRtcSessionUsecase(sessionRepo, messageRepo)
	serverConfigUsecase := usecase.NewServerConfigUsecase(configRepo, auditLogRepo, oauth2UserRepo, db)

	// Set up ban publisher for distributed sync (requires Redis)
	if rdb != nil {
		banPublisher := usecase.NewRedisBanPublisher(rdb)
		rtcUserUsecase.SetBanPublisher(banPublisher)
		logger.Info(ctx, "admin.ban_publisher_enabled")
	} else {
		logger.Warn(ctx, "admin.ban_publisher_disabled_redis_not_available")
	}

	// Inject policy publisher for multi-instance sync (P0 #1 fix)
	if policyWatcher != nil {
		adminRoleUsecase.SetPolicyPublisher(policyWatcher)
		permissionUsecase.SetPolicyPublisher(policyWatcher)
		adminUserRoleUsecase.SetPolicyPublisher(policyWatcher)
		logger.Info(ctx, "admin.policy_publisher_enabled_for_all_usecases")
	} else {
		logger.Info(ctx, "admin.policy_publisher_disabled_single_instance_mode")
	}

	// Init handlers
	adminAuthHandler := httphandler.NewAdminAuthHandler(adminAuthUsecase, jwtSigner, db)
	adminRoleHandler := httphandler.NewAdminRoleHandler(adminRoleUsecase)
	permissionHandler := httphandler.NewPermissionHandler(permissionUsecase)
	adminUserRoleHandler := httphandler.NewAdminUserRoleHandler(adminUserRoleUsecase, adminUserRepo, adminRoleRepo, adminUserRoleRepo, enforcer)
	auditLogHandler := httphandler.NewAuditLogHandler(auditLogRepo)
	adminUserHandler := httphandler.NewAdminUserHandler(adminUserUsecase)
	rtcUserHandler := httphandler.NewRtcUserHandler(rtcUserUsecase)
	rtcSessionHandler := httphandler.NewRtcSessionHandler(rtcSessionUsecase)
	serverConfigHandler := httphandler.NewServerConfigHandler(serverConfigUsecase)
	userConfigHandler := httphandler.NewUserConfigHandler(serverConfigUsecase)
	metricsProxyHandler := httphandler.NewMetricsProxyHandler(cfg.PrometheusURL)
	grafanaProxyHandler := httphandler.NewGrafanaProxyHandler(cfg.GrafanaURL)
	jaegerProxyHandler := httphandler.NewJaegerProxyHandler(cfg.JaegerURL)
	pyroscopeProxyHandler := httphandler.NewPyroscopeProxyHandler(cfg.PyroscopeURL)

	// Wire permission system deps to auth handler
	permissionSystemEnabled := cfg.Features.PermissionSystem
	adminAuthHandler.SetPermissionDeps(adminRoleRepo, adminUserRoleRepo, enforcer, permissionSystemEnabled)

	// Wire email OTP usecase to auth handler (if configured)
	if emailOTPUsecase != nil {
		adminAuthHandler.SetEmailOTPUsecase(emailOTPUsecase)
	}

	// Wire password login configuration to auth handler
	adminAuthHandler.SetPasswordEnabled(cfg.Features.PasswordEnabled)

	// Wire cookie security configuration to auth handler.
	// Enable Secure flag in production HTTPS deployments to prevent cookie leakage over HTTP.
	adminAuthHandler.SetCookieSecure(cfg.Security.CookieSecure)

	// Setup router
	router := setupRouter(routerDeps{
		handlers: handlerDeps{
			adminAuth:      adminAuthHandler,
			adminRole:      adminRoleHandler,
			permission:     permissionHandler,
			adminUserRole:  adminUserRoleHandler,
			auditLog:       auditLogHandler,
			adminUser:      adminUserHandler,
			rtcSession:     rtcSessionHandler,
			rtcUser:        rtcUserHandler,
			serverConfig:   serverConfigHandler,
			userConfig:     userConfigHandler,
			metricsProxy:   metricsProxyHandler,
			grafanaProxy:   grafanaProxyHandler,
			jaegerProxy:    jaegerProxyHandler,
			pyroscopeProxy: pyroscopeProxyHandler,
		},
		auth: authDeps{
			enforcer:          enforcer,
			permissionEnabled: permissionSystemEnabled,
		},
		cors: corsDeps{
			allowedOrigins: cfg.CORS.AllowedOrigins,
			env:            cfg.Server.Env,
		},
		middleware: middlewareDeps{
			rateLimiter: rateLimiter,
		},
	})

	// Create HTTP server with explicit timeouts to prevent resource exhaustion.
	// Without these, the server uses zero values (no timeout), making it
	// vulnerable to Slowloris attacks and connection leaks.
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
		IdleTimeout:  httpIdleTimeout,
	}

	// Start server
	logger.SafeGo("admin-http-server", func() {
		logger.Info(ctx, "admin.server_listening",
			zap.String("addr", addr),
			zap.String("env", cfg.Server.Env),
			zap.Bool("permission_system", permissionSystemEnabled),
			zap.Bool("degraded_mode", bootstrapDegraded.Load()),
			zap.Duration("read_timeout", httpReadTimeout),
			zap.Duration("write_timeout", httpWriteTimeout),
			zap.Duration("idle_timeout", httpIdleTimeout))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(ctx, "admin.server_failed", zap.Error(err))
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		}
	})

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info(ctx, "admin.server_shutting_down")

	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error(ctx, "admin.server_forced_shutdown", zap.Error(err))
	}

	logger.Info(ctx, "admin.server_exited")
}

// handlerDeps groups all HTTP handler dependencies by functional module.
type handlerDeps struct {
	// Admin management handlers
	adminAuth     *httphandler.AdminAuthHandler
	adminRole     *httphandler.AdminRoleHandler
	permission    *httphandler.PermissionHandler
	adminUserRole *httphandler.AdminUserRoleHandler
	auditLog      *httphandler.AuditLogHandler
	adminUser     *httphandler.AdminUserHandler

	// RTC management handlers
	rtcSession *httphandler.RtcSessionHandler
	rtcUser    *httphandler.RtcUserHandler

	// Configuration & monitoring handlers
	serverConfig   *httphandler.ServerConfigHandler
	userConfig     *httphandler.UserConfigHandler
	metricsProxy   *httphandler.MetricsProxyHandler
	grafanaProxy   *httphandler.GrafanaProxyHandler
	jaegerProxy    *httphandler.JaegerProxyHandler
	pyroscopeProxy *httphandler.PyroscopeProxyHandler
}

// authDeps groups authorization-related dependencies.
type authDeps struct {
	enforcer          *auth.CasbinEnforcer
	permissionEnabled bool
}

// corsDeps groups CORS configuration dependencies.
type corsDeps struct {
	allowedOrigins []string
	// env is the runtime environment (development/production).
	// In production, an empty allowedOrigins list blocks all cross-origin
	// requests instead of allowing all origins (secure-by-default).
	env string
}

// middlewareDeps groups middleware-related dependencies.
type middlewareDeps struct {
	rateLimiter httphandler.RateLimiterInterface
}

// routerDeps groups all dependencies needed to configure the Gin router,
// organized by functional module to improve readability.
type routerDeps struct {
	handlers   handlerDeps
	auth       authDeps
	cors       corsDeps
	middleware middlewareDeps
}

// setupRouter creates and configures the Gin router with all routes and middleware.
func setupRouter(deps routerDeps) *gin.Engine {
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()

	// Configure trusted proxies for correct ClientIP() extraction.
	// In Docker deployment, Nginx is the reverse proxy, so we trust its IP.
	// For development, trust all private network ranges.
	if err := router.SetTrustedProxies([]string{
		"127.0.0.0/8",    // localhost
		"10.0.0.0/8",     // Docker bridge network
		"172.16.0.0/12",  // Docker custom networks
		"192.168.0.0/16", // Docker bridge network
	}); err != nil {
		// Log but don't fail - ClientIP() will fall back to RemoteAddr
		logger.Warn(context.Background(), "Failed to set trusted proxies", zap.Error(err))
	}

	// Recovery middleware
	router.Use(gin.Recovery())

	// Request ID middleware — generates/propagates X-Request-ID for tracing
	router.Use(httphandler.AdminRequestIDMiddleware())

	// Logger middleware
	router.Use(gin.Logger())

	// Security headers middleware (P0 #8)
	router.Use(httphandler.SecurityHeadersMiddleware())

	// Rate limiting middleware (P0 #2) — applied globally
	router.Use(httphandler.AdminRateLimitMiddleware(deps.middleware.rateLimiter))

	// CORS middleware
	//
	// Security behavior by environment:
	// - production/staging: empty allowedOrigins blocks ALL cross-origin requests
	//   (secure-by-default; operators must explicitly configure CORS origins).
	// - development: empty allowedOrigins allows all origins for developer ergonomics.
	//
	// This prevents accidental open CORS in production when the config is missing.
	isProduction := deps.cors.env == "production" || deps.cors.env == "staging"
	if isProduction && len(deps.cors.allowedOrigins) == 0 {
		logger.Warn(context.Background(),
			"admin.cors_empty_origins_in_production_blocking_all_cross_origin_requests",
			zap.String("env", deps.cors.env))
	}

	router.Use(func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		allowed := false

		if len(deps.cors.allowedOrigins) == 0 {
			// In production/staging, empty origins means "block all".
			// In development, empty origins means "allow all" (backward compatible).
			if !isProduction {
				allowed = true
			}
			// If production and empty, allowed stays false — no CORS headers sent.
		} else {
			for _, o := range deps.cors.allowedOrigins {
				if o == origin {
					allowed = true
					break
				}
			}
		}

		if allowed {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Vary", "Origin")
			c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	})

	// Register readiness probe endpoint.
	// Reports degraded status when bootstrap failed, allowing load balancers
	// to route traffic away from unhealthy instances.
	router.GET("/ready", func(c *gin.Context) {
		if bootstrapDegraded.Load() {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "degraded",
				"error":  "bootstrap failed — default roles may be missing",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status": "ready",
		})
	})

	// Register admin auth routes (public + protected)
	deps.handlers.adminAuth.RegisterRoutes(router)

	// Protected API routes with JWT + Casbin middleware
	apiGroup := router.Group("/api")
	apiGroup.Use(deps.handlers.adminAuth.JWTAuthMiddleware())
	apiGroup.Use(httphandler.CasbinMiddleware(deps.auth.enforcer, deps.auth.permissionEnabled))

	// Register management routes
	deps.handlers.adminRole.RegisterRoutes(apiGroup)
	deps.handlers.permission.RegisterRoutes(apiGroup)
	deps.handlers.adminUserRole.RegisterRoutes(apiGroup)
	deps.handlers.auditLog.RegisterRoutes(apiGroup)
	deps.handlers.adminUser.RegisterRoutes(apiGroup)
	deps.handlers.rtcSession.RegisterRoutes(apiGroup)
	deps.handlers.rtcUser.RegisterRoutes(apiGroup)
	deps.handlers.serverConfig.RegisterRoutes(apiGroup)
	deps.handlers.userConfig.RegisterRoutes(apiGroup)
	deps.handlers.metricsProxy.RegisterRoutes(apiGroup)
	deps.handlers.grafanaProxy.RegisterRoutes(apiGroup)
	deps.handlers.jaegerProxy.RegisterRoutes(apiGroup)
	deps.handlers.pyroscopeProxy.RegisterRoutes(apiGroup)

	// Register static file server for admin-ui (SPA)
	ServeStaticFiles(router)

	return router
}

// parseInstanceCount reads the INSTANCE_COUNT environment variable to determine
// the expected number of server replicas. Returns 1 if unset or invalid.
// This is used to warn operators when multi-instance features are needed but
// supporting infrastructure (Redis) is unavailable.
func parseInstanceCount() int {
	raw := os.Getenv("INSTANCE_COUNT")
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// populateDynamicConfigDefaults loads the main server config from the default
// path and populates the dynamic config registry with actual yaml values.
// This ensures the admin UI (via ServerConfigUsecase) displays the real yaml
// defaults rather than the hardcoded registry fallbacks.
//
// The admin server shares the deployment with the main server, so the main
// config file is available at the standard location (etc/config.yaml).
// If loading fails (e.g., file not found), the function logs a warning and
// the admin server continues with the hardcoded registry defaults -- this is
// non-fatal because the registry always has sensible fallback values.
func populateDynamicConfigDefaults() {
	mainCfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load main config for yaml defaults: %v\n", err)
		return
	}
	config.PopulateYamlDefaults(mainCfg)
}
