// Package admin provides the admin-server cobra command and initialization.
package admin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
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
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
)

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the admin server",
	Long:  `Start the RTC Agent admin server for admin user management and authentication`,
	Run:   runServe,
}

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

	// Init logger
	logger.Init("info", "")
	defer logger.Sync()

	ctx := context.Background()
	logger.Info(ctx, "admin.starting")

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

	// Bootstrap default roles and policies (idempotent, transactional) (P0 #4)
	// If bootstrap fails, log error but continue - server can still function in degraded mode
	if err := usecase.BootstrapAdmin(ctx, db, adminRoleRepo, enforcer); err != nil {
		logger.Error(ctx, "admin.bootstrap_admin_failed_default_roles_may_be_missing",
			zap.Error(err))
		// Don't exit - allow server to start even if bootstrap failed
		// Admin can manually fix issues or restart after resolving DB problems
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

	// Init usecases
	adminAuthUsecase := usecase.NewAdminAuthUsecase(adminUserRepo, refreshTokenRepo, jwtSigner)
	adminAuthUsecase.SetLoginProtection(loginProtection)
	adminRoleUsecase := usecase.NewAdminRoleUsecase(adminRoleRepo, adminUserRoleRepo, auditLogRepo, enforcer)
	permissionUsecase := usecase.NewPermissionUsecase(adminRoleRepo, enforcer, auditLogRepo)
	adminUserRoleUsecase := usecase.NewAdminUserRoleUsecase(adminUserRepo, adminRoleRepo, adminUserRoleRepo, enforcer, auditLogRepo)
	adminUserUsecase := usecase.NewAdminUserUsecase(adminUserRepo)
	rtcUserUsecase := usecase.NewRtcUserUsecase(db, oauth2UserRepo, mainRefreshTokenRepo, auditLogRepo)

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

	// Wire permission system deps to auth handler
	permissionSystemEnabled := cfg.Features.PermissionSystem
	adminAuthHandler.SetPermissionDeps(adminRoleRepo, adminUserRoleRepo, enforcer, permissionSystemEnabled)

	// Setup router
	router := setupRouter(routerDeps{
		adminAuthHandler:     adminAuthHandler,
		adminRoleHandler:     adminRoleHandler,
		permissionHandler:    permissionHandler,
		adminUserRoleHandler: adminUserRoleHandler,
		auditLogHandler:      auditLogHandler,
		adminUserHandler:     adminUserHandler,
		rtcUserHandler:       rtcUserHandler,
		enforcer:             enforcer,
		permissionEnabled:    permissionSystemEnabled,
		allowedOrigins:       cfg.CORS.AllowedOrigins,
		rateLimiter:          rateLimiter,
	})

	// Create HTTP server
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: router,
	}

	// Start server
	logger.SafeGo("admin-http-server", func() {
		logger.Info(ctx, "admin.server_listening",
			zap.String("addr", addr),
			zap.String("env", cfg.Server.Env),
			zap.Bool("permission_system", permissionSystemEnabled))
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

// routerDeps groups all dependencies needed to configure the Gin router.
type routerDeps struct {
	adminAuthHandler     *httphandler.AdminAuthHandler
	adminRoleHandler     *httphandler.AdminRoleHandler
	permissionHandler    *httphandler.PermissionHandler
	adminUserRoleHandler *httphandler.AdminUserRoleHandler
	auditLogHandler      *httphandler.AuditLogHandler
	adminUserHandler     *httphandler.AdminUserHandler
	rtcUserHandler       *httphandler.RtcUserHandler
	enforcer             *auth.CasbinEnforcer
	permissionEnabled    bool
	allowedOrigins       []string
	rateLimiter          httphandler.RateLimiterInterface
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

	// Logger middleware
	router.Use(gin.Logger())

	// Security headers middleware (P0 #8)
	router.Use(httphandler.SecurityHeadersMiddleware())

	// Rate limiting middleware (P0 #2) — applied globally
	router.Use(httphandler.AdminRateLimitMiddleware(deps.rateLimiter))

	// CORS middleware
	router.Use(func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		allowed := false
		if len(deps.allowedOrigins) == 0 {
			allowed = true
		} else {
			for _, o := range deps.allowedOrigins {
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

	// Register admin auth routes (public + protected)
	deps.adminAuthHandler.RegisterRoutes(router)

	// Protected API routes with JWT + Casbin middleware
	apiGroup := router.Group("/api")
	apiGroup.Use(deps.adminAuthHandler.JWTAuthMiddleware())
	apiGroup.Use(httphandler.CasbinMiddleware(deps.enforcer, deps.permissionEnabled))

	// Register management routes
	deps.adminRoleHandler.RegisterRoutes(apiGroup)
	deps.permissionHandler.RegisterRoutes(apiGroup)
	deps.adminUserRoleHandler.RegisterRoutes(apiGroup)
	deps.auditLogHandler.RegisterRoutes(apiGroup)
	deps.adminUserHandler.RegisterRoutes(apiGroup)
	deps.rtcUserHandler.RegisterRoutes(apiGroup)

	// Register static file server for admin-ui (SPA)
	ServeStaticFiles(router)

	return router
}
