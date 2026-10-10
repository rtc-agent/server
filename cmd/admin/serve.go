// Package admin provides the admin-server cobra command and initialization.
package admin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
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
	// Load and validate config
	cfg, err := config.LoadAdminConfig(adminCfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load admin config: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid admin config: %v\n", err)
		os.Exit(1)
	}

	// Populate dynamic config registry with yaml defaults
	populateDynamicConfigDefaults()

	// Init logger
	logger.Init("info", "")
	defer logger.Sync()

	ctx := context.Background()
	logger.Info(ctx, "admin.starting")

	// Log deployment mode
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

	// Init Redis (optional)
	rdb := initRedis(ctx, cfg)

	// Warn if multi-instance deployment is configured but Redis is unavailable
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

	// Init repositories and usecases
	repos := initRepositories(db)
	usecases := initUsecases(ctx, db, cfg, rdb, repos, enforcer)

	// Init handlers
	handlers := initHandlers(cfg, usecases, jwtSigner, db)

	// Setup router
	router := setupRouter(routerDeps{
		handlers: handlers,
		auth: authDeps{
			enforcer:          enforcer,
			permissionEnabled: cfg.Features.PermissionSystem,
		},
		cors: corsDeps{
			allowedOrigins: cfg.CORS.AllowedOrigins,
			env:            cfg.Server.Env,
		},
		middleware: middlewareDeps{
			rateLimiter: initRateLimiter(ctx, cfg, rdb),
		},
		security: securityDeps{
			cspConfig: cfg.Security.CSP,
		},
	})

	// Create HTTP server with explicit timeouts
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
			zap.Bool("permission_system", cfg.Features.PermissionSystem),
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

	// Clean up EmailOTPUsecase resources (stops MemoryOTPStore cleanup goroutine if present)
	if usecases.emailOTPUsecase != nil {
		usecases.emailOTPUsecase.Close()
	}

	logger.Info(ctx, "admin.server_exited")
}

// initRedis initializes Redis client if configured.
func initRedis(ctx context.Context, cfg *config.AdminConfig) *redis.Client {
	if cfg.Redis.Addr == "" {
		logger.Info(ctx, "admin.redis_not_configured_jwks_caching_disabled")
		return nil
	}

	rdb := redis.NewClient(&redis.Options{
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
		return nil
	}

	logger.Info(ctx, "admin.redis_connected", zap.String("addr", cfg.Redis.Addr))
	return rdb
}

// repositories groups all repository dependencies.
type repositories struct {
	adminUserRepo        repo.AdminUserRepo
	refreshTokenRepo     repo.AdminRefreshTokenRepo
	adminRoleRepo        repo.AdminRoleRepo
	adminUserRoleRepo    repo.AdminUserRoleRepo
	auditLogRepo         repo.AuditLogRepo
	oauth2UserRepo       repo.OAuth2UserRepo
	mainRefreshTokenRepo repo.RefreshTokenRepo
	configRepo           repo.ServerConfigRepo
	deviceRepo           repo.DeviceRepo
	sessionRepo          repo.SessionRepo
	messageRepo          repo.MessageRepo
	turnRepo             repo.TurnRepo
}

// initRepositories initializes all repository dependencies.
func initRepositories(db *gorm.DB) repositories {
	return repositories{
		adminUserRepo:        repo.NewAdminUserRepo(db),
		refreshTokenRepo:     repo.NewAdminRefreshTokenRepo(db),
		adminRoleRepo:        repo.NewAdminRoleRepo(db),
		adminUserRoleRepo:    repo.NewAdminUserRoleRepo(db),
		auditLogRepo:         repo.NewAuditLogRepo(db),
		oauth2UserRepo:       repo.NewOAuth2UserRepo(db),
		mainRefreshTokenRepo: repo.NewRefreshTokenRepo(db),
		configRepo:           repo.NewServerConfigRepo(db),
		deviceRepo:           repo.NewDeviceRepo(db),
		sessionRepo:          repo.NewSessionRepo(db),
		messageRepo:          repo.NewMessageRepo(db),
		turnRepo:             repo.NewTurnRepo(db),
	}
}

// usecases groups all usecase dependencies.
type usecases struct {
	adminAuthUsecase     *usecase.AdminAuthUsecase
	adminRoleUsecase     *usecase.AdminRoleUsecase
	permissionUsecase    *usecase.PermissionUsecase
	adminUserRoleUsecase *usecase.AdminUserRoleUsecase
	adminUserUsecase     *usecase.AdminUserUsecase
	rtcUserUsecase       *usecase.RtcUserUsecase
	rtcSessionUsecase    *usecase.RtcSessionUsecase
	serverConfigUsecase  *usecase.ServerConfigUsecase
	auditLogUsecase      *usecase.AuditLogUsecase
	loginProtection      usecase.LoginProtectionInterface
	emailOTPUsecase      *usecase.EmailOTPUsecase
}

// initUsecases initializes all usecase dependencies.
func initUsecases(ctx context.Context, db *gorm.DB, cfg *config.AdminConfig, rdb *redis.Client, repos repositories, enforcer *auth.CasbinEnforcer) usecases {
	// Bootstrap default roles and policies
	if err := usecase.BootstrapAdmin(ctx, db, repos.adminRoleRepo, enforcer); err != nil {
		bootstrapDegraded.Store(true)
		logger.Error(ctx, "admin.bootstrap_failed_degraded_mode_default_roles_may_be_missing", zap.Error(err))
	} else {
		logger.Info(ctx, "admin.bootstrap_completed")
	}

	// Init rate limiter
	rateLimiter := initRateLimiter(ctx, cfg, rdb)
	_ = rateLimiter // rateLimiter is used in router setup

	// Init login protection
	loginProtection := initLoginProtection(ctx, cfg, rdb)
	defer loginProtection.Stop()

	// Init email sender (optional)
	emailSender := initEmailSender(ctx, cfg)

	// Init OTP usecase (requires email sender)
	var emailOTPUsecase *usecase.EmailOTPUsecase
	if emailSender != nil {
		emailOTPUsecase = initEmailOTPUsecase(ctx, cfg, rdb, repos.adminUserRepo, emailSender)
	}

	// Init usecases
	adminAuthUsecase := usecase.NewAdminAuthUsecase(db, repos.adminUserRepo, repos.refreshTokenRepo, nil) // jwtSigner will be set later
	adminAuthUsecase.SetLoginProtection(loginProtection)
	adminRoleUsecase := usecase.NewAdminRoleUsecase(db, repos.adminRoleRepo, repos.adminUserRoleRepo, repos.auditLogRepo, enforcer)
	permissionUsecase := usecase.NewPermissionUsecase(repos.adminRoleRepo, enforcer, repos.auditLogRepo)
	adminUserRoleUsecase := usecase.NewAdminUserRoleUsecase(db, repos.adminUserRepo, repos.adminRoleRepo, repos.adminUserRoleRepo, enforcer, repos.auditLogRepo)
	adminUserUsecase := usecase.NewAdminUserUsecase(db, repos.adminUserRepo, repos.adminUserRoleRepo, repos.adminRoleRepo, enforcer)
	rtcUserUsecase := usecase.NewRtcUserUsecase(db, repos.oauth2UserRepo, repos.mainRefreshTokenRepo, repos.auditLogRepo, repos.deviceRepo)
	rtcSessionUsecase := usecase.NewRtcSessionUsecase(repos.sessionRepo, repos.messageRepo, repos.turnRepo)
	serverConfigUsecase := usecase.NewServerConfigUsecase(repos.configRepo, repos.auditLogRepo, repos.oauth2UserRepo, db)
	auditLogUsecase := usecase.NewAuditLogUsecase(repos.auditLogRepo)

	// Set up config publisher for multi-instance cache synchronization
	if rdb != nil {
		configPublisher := usecase.NewRedisConfigPublisher(rdb)
		serverConfigUsecase.SetConfigPublisher(configPublisher)
		logger.Info(ctx, "admin.config_publisher_enabled")
	} else {
		logger.Warn(ctx, "admin.config_publisher_disabled_redis_not_available")
	}

	// Set up ban publisher for distributed sync (requires Redis)
	if rdb != nil {
		banPublisher := usecase.NewRedisBanPublisher(rdb)
		rtcUserUsecase.SetBanPublisher(banPublisher)
		logger.Info(ctx, "admin.ban_publisher_enabled")
	} else {
		logger.Warn(ctx, "admin.ban_publisher_disabled_redis_not_available")
	}

	return usecases{
		adminAuthUsecase:     adminAuthUsecase,
		adminRoleUsecase:     adminRoleUsecase,
		permissionUsecase:    permissionUsecase,
		adminUserRoleUsecase: adminUserRoleUsecase,
		adminUserUsecase:     adminUserUsecase,
		rtcUserUsecase:       rtcUserUsecase,
		rtcSessionUsecase:    rtcSessionUsecase,
		serverConfigUsecase:  serverConfigUsecase,
		auditLogUsecase:      auditLogUsecase,
		loginProtection:      loginProtection,
		emailOTPUsecase:      emailOTPUsecase,
	}
}

// initRateLimiter initializes the rate limiter (Redis-backed or memory-based).
func initRateLimiter(ctx context.Context, cfg *config.AdminConfig, rdb *redis.Client) httphandler.RateLimiterInterface {
	if rdb != nil {
		logger.Info(ctx, "admin.redis_rate_limiter_enabled")
		return httphandler.NewRedisRateLimiter(rdb, cfg.Security.RateLimitPerMinute)
	}
	memLimiter := httphandler.NewRateLimiter(httphandler.RateLimitConfig{
		MaxRequestsPerMinute: cfg.Security.RateLimitPerMinute,
	})
	defer memLimiter.Stop()
	logger.Info(ctx, "admin.memory_rate_limiter_enabled_single_instance_only")
	return memLimiter
}

// initLoginProtection initializes login protection (Redis-backed or memory-based).
func initLoginProtection(ctx context.Context, cfg *config.AdminConfig, rdb *redis.Client) usecase.LoginProtectionInterface {
	lpCfg := usecase.LoginProtectionConfig{
		MaxAttempts:  cfg.Security.LoginMaxAttempts,
		LockDuration: time.Duration(cfg.Security.LoginLockDuration) * time.Second,
	}
	if rdb != nil {
		logger.Info(ctx, "admin.redis_login_protection_enabled")
		return usecase.NewRedisLoginProtection(rdb, lpCfg)
	}
	// Memory-based login protection does not share state across instances.
	// In production with multiple replicas, an attacker can rotate requests
	// across instances to bypass lockout. Warn so operators can enable Redis.
	if cfg.Server.Env == "production" {
		logger.Warn(ctx, "admin.memory_login_protection_unsafe_in_multi_instance_deployments_consider_redis")
	} else {
		logger.Info(ctx, "admin.memory_login_protection_enabled_single_instance_only")
	}
	return usecase.NewLoginProtection(lpCfg)
}

// initEmailSender initializes the email sender if configured.
func initEmailSender(ctx context.Context, cfg *config.AdminConfig) email.Sender {
	if cfg.Email.SMTPHost == "" || cfg.Email.FromAddress == "" {
		logger.Warn(ctx, "admin.email_not_configured_otp_login_disabled")
		return nil
	}
	logger.Info(ctx, "admin.smtp_email_sender_configured",
		zap.String("host", cfg.Email.SMTPHost),
		zap.String("from", cfg.Email.FromAddress))
	return email.NewSMTPSender(email.SMTPConfig{
		Host:        cfg.Email.SMTPHost,
		Port:        cfg.Email.SMTPPort,
		User:        cfg.Email.SMTPUser,
		Password:    cfg.Email.SMTPPassword,
		FromAddress: cfg.Email.FromAddress,
		FromName:    cfg.Email.FromName,
	})
}

// initEmailOTPUsecase initializes the email OTP usecase.
func initEmailOTPUsecase(ctx context.Context, cfg *config.AdminConfig, rdb *redis.Client, adminUserRepo repo.AdminUserRepo, emailSender email.Sender) *usecase.EmailOTPUsecase {
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

	logger.Info(ctx, "admin.email_otp_usecase_enabled")
	return usecase.NewEmailOTPUsecase(adminUserRepo, emailSender, otpStore, otpCfg)
}

// initHandlers initializes all HTTP handlers.
func initHandlers(cfg *config.AdminConfig, usecases usecases, jwtSigner *auth.AdminJWTSigner, db *gorm.DB) handlerDeps {
	adminAuthHandler := httphandler.NewAdminAuthHandler(usecases.adminAuthUsecase, usecases.adminUserUsecase, jwtSigner, db)
	adminRoleHandler := httphandler.NewAdminRoleHandler(usecases.adminRoleUsecase)
	permissionHandler := httphandler.NewPermissionHandler(usecases.permissionUsecase)
	adminUserRoleHandler := httphandler.NewAdminUserRoleHandler(usecases.adminUserRoleUsecase)
	auditLogHandler := httphandler.NewAuditLogHandler(usecases.auditLogUsecase)
	adminUserHandler := httphandler.NewAdminUserHandler(usecases.adminUserUsecase)
	rtcUserHandler := httphandler.NewRtcUserHandler(usecases.rtcUserUsecase)
	rtcSessionHandler := httphandler.NewRtcSessionHandler(usecases.rtcSessionUsecase)
	serverConfigHandler := httphandler.NewServerConfigHandler(usecases.serverConfigUsecase)
	userConfigHandler := httphandler.NewUserConfigHandler(usecases.serverConfigUsecase)
	metricsProxyHandler := httphandler.NewMetricsProxyHandler(cfg.PrometheusURL)
	grafanaProxyHandler := httphandler.NewGrafanaProxyHandler(cfg.GrafanaURL)
	jaegerProxyHandler := httphandler.NewJaegerProxyHandler(cfg.JaegerURL)
	pyroscopeProxyHandler := httphandler.NewPyroscopeProxyHandler(cfg.PyroscopeURL)

	// Set permission system enabled flag on auth handler
	adminAuthHandler.SetPermissionSystemEnabled(cfg.Features.PermissionSystem)

	// Wire email OTP usecase to auth handler (if configured)
	if usecases.emailOTPUsecase != nil {
		adminAuthHandler.SetEmailOTPUsecase(usecases.emailOTPUsecase)
	}

	// Wire password login configuration to auth handler
	adminAuthHandler.SetPasswordEnabled(cfg.Features.PasswordEnabled)

	// Wire cookie security configuration to auth handler
	adminAuthHandler.SetCookieSecure(cfg.Security.CookieSecure)

	return handlerDeps{
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
	}
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

// securityDeps groups security-related configuration dependencies.
type securityDeps struct {
	// cspConfig holds Content Security Policy configuration.
	cspConfig config.CSPConfig
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
	security   securityDeps
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

	// Security headers middleware
	router.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Next()
	})

	// CSP middleware (Content Security Policy)
	// Note: CSP provides more granular control than X-Frame-Options.
	// When CSP frame-ancestors is set, it takes precedence over X-Frame-Options.
	router.Use(middleware.GinCSP(&deps.security.cspConfig))

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
				if strings.EqualFold(o, origin) {
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
			c.Writer.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
			c.Writer.Header().Set("Access-Control-Max-Age", "86400") // 24 hours
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

	// Register CSP violation report endpoint (no auth required).
	// Browsers send violation reports to this endpoint when CSP is enabled.
	if deps.security.cspConfig.Enabled && deps.security.cspConfig.ReportURI != "" {
		router.POST(deps.security.cspConfig.ReportURI, httphandler.HandleGinCSPReport)
	}

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
