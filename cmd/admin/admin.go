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
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	httphandler "github.com/rtc-agent/server/internal/handler/http"
	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
)

var (
	adminCfgFile string
)

// adminCmd represents the admin command
var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Start the admin server",
	Long:  `Start the RTC Agent admin server for user management and authentication`,
	Run:   runAdmin,
}

// init initializes the admin command
func init() {
	adminCmd.PersistentFlags().StringVar(&adminCfgFile, "config", "etc/admin.yaml", "config file (default is etc/admin.yaml)")
}

// GetCommand returns the admin cobra command
func GetCommand() *cobra.Command {
	return adminCmd
}

func runAdmin(cmd *cobra.Command, args []string) {
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

	logger.Info(context.Background(), "Starting RTC Agent admin server...")

	// Init database
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.NewGormLogger(true, 200*time.Millisecond),
	})
	if err != nil {
		logger.Fatal(context.Background(), "Failed to connect database", zap.Error(err))
	}

	// Auto-migrate schema if configured
	if cfg.Database.AutoMigrate {
		if err := autoMigrate(db); err != nil {
			logger.Fatal(context.Background(), "Failed to auto-migrate database", zap.Error(err))
		}
	}

	// Init Redis (optional — JWKS caching and distributed rate limiting require it).
	// In development, Redis may be unavailable; only connect when address is configured.
	// TODO: pass rdb to JWKSClient and rate limiter once those subsystems are wired up.
	if cfg.Redis.Addr != "" {
		rdb := redis.NewClient(&redis.Options{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		})
		defer func() { _ = rdb.Close() }()
		// Verify connectivity at startup; fail fast if Redis is unreachable.
		if err := rdb.Ping(context.Background()).Err(); err != nil {
			logger.Warn(context.Background(), "Redis configured but unreachable — continuing without distributed cache",
				zap.String("addr", cfg.Redis.Addr), zap.Error(err))
		} else {
			logger.Info(context.Background(), "Redis connected",
				zap.String("addr", cfg.Redis.Addr))
		}
	} else {
		logger.Info(context.Background(), "Redis not configured — JWKS caching and distributed rate limiting disabled")
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
		logger.Fatal(context.Background(), "Failed to initialize JWT signer", zap.Error(err))
	}

	// Init repositories
	userRepo := repo.NewUserRepo(db)
	refreshTokenRepo := repo.NewRefreshTokenRepo(db)

	// Init usecase
	adminAuthUsecase := usecase.NewAdminAuthUsecase(userRepo, refreshTokenRepo, jwtSigner)

	// Init handler
	adminAuthHandler := httphandler.NewAdminAuthHandler(adminAuthUsecase, jwtSigner, db)

	// Setup router
	router := setupRouter(adminAuthHandler, cfg.Server.AllowedOrigins)

	// Create HTTP server
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: router,
	}

	// Start server
	logger.SafeGo("admin-http-server", func() {
		logger.Info(context.Background(), "Admin server listening",
			zap.String("addr", addr),
			zap.String("env", cfg.Server.Env))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(context.Background(), "Admin server failed", zap.Error(err))
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		}
	})

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info(context.Background(), "Shutting down admin server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error(context.Background(), "Admin server forced to shutdown", zap.Error(err))
	}

	logger.Info(context.Background(), "Admin server exited")
}

// setupRouter creates and configures the Gin router
func setupRouter(adminAuthHandler *httphandler.AdminAuthHandler, allowedOrigins []string) *gin.Engine {
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()

	// Recovery middleware
	router.Use(gin.Recovery())

	// Logger middleware
	router.Use(gin.Logger())

	// CORS middleware — restrict origins in production, allow all in development.
	// When allowedOrigins is empty, all origins are permitted (development convenience).
	// Production deployments MUST configure explicit origins to prevent cross-origin attacks.
	router.Use(func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		allowed := false
		if len(allowedOrigins) == 0 {
			// Development mode: allow all origins
			allowed = true
		} else {
			for _, o := range allowedOrigins {
				if o == origin {
					allowed = true
					break
				}
			}
		}

		if allowed {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Vary", "Origin")
			c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	})

	// Register admin auth routes
	adminAuthHandler.RegisterRoutes(router)

	return router
}

// autoMigrate runs database migrations for admin-server models
func autoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&model.User{},
		&model.RefreshToken{},
	)
}
