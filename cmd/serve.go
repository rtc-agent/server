package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rtc-agent/server/internal/infra/config"
	dbmetrics "github.com/rtc-agent/server/internal/infra/db"
	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/infra/tracing"
	"github.com/rtc-agent/server/pkg/logger"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the server",
	Long:  `Start the RTC Agent server`,
	Run:   runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) {
	// Load config
	cfg, err := config.Load(cfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Validate required config fields
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid config: %v\n", err)
		os.Exit(1)
	}

	// Populate dynamic config registry with yaml defaults so that
	// config.GetRegistryEntry().YamlDefault reflects the actual yaml values.
	config.PopulateYamlDefaults(cfg)

	// SECURITY CHECK: Validate dev bypass safety before proceeding
	if warning := cfg.ValidateDevBypassSafety(); warning != "" {
		fmt.Fprintf(os.Stderr, "[SECURITY] %s\n", warning)
		os.Exit(1)
	}

	// Init logger
	logger.Init(cfg.Log.Level, cfg.Log.ServerLogFile)
	defer logger.Sync()

	// SECURITY WARNING: Log when JWT dev bypass is enabled
	// This is a critical security feature that allows skipping JWT validation
	if cfg.IsDevBypassAllowed() {
		logger.Warn(context.Background(),
			"JWT DEV BYPASS ENABLED: X-User-ID/X-Device-ID headers will bypass JWT validation",
			zap.String("environment", cfg.Server.Env),
			zap.String("warning", "This feature must NEVER be enabled in production"),
			zap.String("recommendation", "Set server.env=production to disable dev bypass"),
		)
		// Also print to stderr for visibility during startup
		fmt.Fprintf(os.Stderr, "[SECURITY] WARNING: JWT dev bypass is ENABLED (environment=%s)\n", cfg.Server.Env)
		fmt.Fprintf(os.Stderr, "[SECURITY] Requests can bypass JWT authentication using X-User-ID/X-Device-ID headers\n")
		fmt.Fprintf(os.Stderr, "[SECURITY] This is ONLY appropriate for local development\n")
	} else {
		logger.Info(context.Background(),
			"JWT dev bypass is DISABLED - all requests require valid JWT tokens",
			zap.String("environment", cfg.Server.Env),
		)
	}

	// Init LLM payload logger (dev only — writes full LLM request/response to logs/llm-payload.log)
	logger.InitLLMPayloadLogger(logger.LLMPayloadConfig{
		Enabled:  cfg.Log.LLMPayload,
		LogLevel: cfg.Log.Level,
	})
	defer logger.SyncLLMPayload()

	logger.Info(context.Background(), "Starting RTC Agent server...")
	if logger.IsDebugMode() {
		logger.Info(context.Background(), "DEBUG mode enabled — extra logs writing to logs/debug.log")
	}
	if cfg.Log.LLMPayload {
		logger.Info(context.Background(), "LLM payload logging enabled — writing to logs/llm-payload.log")
	}
	if cfg.Log.ServerLogFile != "" {
		logger.Info(context.Background(), "Server log file enabled", zap.String("log_file", cfg.Log.ServerLogFile))
	}

	// Init tracing (OpenTelemetry + Jaeger)
	shutdownTracing := initTracing(cfg)
	defer func() {
		if shutdownTracing != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdownTracing(ctx); err != nil {
				logger.Error(context.Background(), "Failed to shutdown tracing", zap.Error(err))
			}
		}
	}()

	// Init database
	db, sqlDB, err := initDatabase(cfg)
	if err != nil {
		logger.Fatal(context.Background(), "Failed to init database", zap.Error(err))
	}
	defer func() {
		logger.Info(context.Background(), "shutdown: closing database connections")
		if err := sqlDB.Close(); err != nil {
			logger.Error(context.Background(), "shutdown: database close error", zap.Error(err))
		}
	}()

	// Init Redis
	rdb, err := initRedis(cfg)
	if err != nil {
		logger.Fatal(context.Background(), "Failed to init Redis", zap.Error(err))
	}
	defer func() {
		logger.Info(context.Background(), "shutdown: closing Redis connection")
		if err := rdb.Close(); err != nil {
			logger.Error(context.Background(), "shutdown: Redis close error", zap.Error(err))
		}
	}()

	// Init ban cache (Redis-backed, shared across all instances)
	middleware.InitBanCache(rdb, 30*time.Second)

	// Init server (Wire-generated)
	srv, err := InitializeServer(cfg, db, rdb)
	if err != nil {
		logger.Fatal(context.Background(), "Failed to initialize server", zap.Error(err))
	}
	logger.SafeGo("http-server", func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			logger.Error(context.Background(), "Server failed", zap.Error(err))
			// Clean up background services (queue-worker, asynq-worker, etc.)
			// that were started before ListenAndServe() failed.
			srv.Stop()
			// Signal the main goroutine to exit.
			// Using signal.Notify + kill self preserves the deferred cleanup
			// (logger.Sync, rdb.Close) that logger.Fatal would bypass.
			// Kill errors are logged but not actionable: the process is already in error state.
			if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
				logger.Error(context.Background(), "Failed to send SIGTERM signal", zap.Error(err))
			}
		}
	})

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	logger.Info(context.Background(), "Shutting down server...", zap.String("signal", sig.String()))

	// Create a context with timeout for graceful shutdown.
	// This ensures that even if some components fail to shut down cleanly,
	// the process will eventually exit.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Shutdown sequence (all steps happen inside srv.Stop()):
	// 1. HTTP server shutdown (drains in-flight requests, stops accepting new connections)
	// 2. rtc-queue Worker stop (cancels session processors, waits with timeout)
	// 3. asynq server stop
	// 4. Background goroutines stop (scanners, cleanup, watchers, config subscriber)
	// 5. BanWatcher close
	// 6. RPC handler close
	// 7. Centrifuge node shutdown + Broker close
	// Note: database and Redis connections are closed by deferred functions.
	logger.Info(shutdownCtx, "shutdown: stopping server components")
	srv.Stop()

	logger.Info(shutdownCtx, "shutdown: complete")
}

// initTracing initializes OpenTelemetry tracing if enabled.
// Returns a shutdown function that should be called on exit.
func initTracing(cfg *config.Config) func(context.Context) error {
	if !cfg.Tracing.Enabled {
		return nil
	}

	// Resolve service version: config > environment variable > "unknown"
	serviceVersion := cfg.Tracing.ServiceVersion
	if serviceVersion == "" {
		serviceVersion = os.Getenv("SERVICE_VERSION")
		if serviceVersion == "" {
			serviceVersion = "unknown"
		}
	}

	// Resolve environment: config > server.env > "development"
	environment := cfg.Tracing.Environment
	if environment == "" {
		environment = cfg.Server.Env
		if environment == "" {
			environment = "development"
		}
	}

	shutdownTracing, err := tracing.Init(tracing.Config{
		ServiceName:    "rtc-agent",
		ServiceVersion: serviceVersion,
		Environment:    environment,
		Endpoint:       cfg.Tracing.Endpoint,
		Insecure:       cfg.Tracing.Insecure,
		SampleRate:     cfg.Tracing.SampleRate,
	})
	if err != nil {
		logger.Fatal(context.Background(), "Failed to initialize tracing", zap.Error(err))
	}
	logger.Info(context.Background(), "Tracing enabled")
	logger.Debug(context.Background(), "Tracing configuration",
		zap.String("endpoint", cfg.Tracing.Endpoint),
		zap.Float64("sample_rate", cfg.Tracing.SampleRate),
		zap.Bool("insecure", cfg.Tracing.Insecure),
		zap.String("service_version", serviceVersion),
		zap.String("environment", environment),
	)
	return shutdownTracing
}

// initDatabase initializes the database connection and configures connection pool.
// Returns the gorm.DB and underlying sql.DB for lifecycle management.
func initDatabase(cfg *config.Config) (*gorm.DB, *sql.DB, error) {
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.NewGormLogger(
			true,                 // ignore ErrRecordNotFound
			200*time.Millisecond, // slow query threshold
		),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("connect database: %w", err)
	}

	// Register GORM tracing plugin for OpenTelemetry spans on SQL operations.
	if err := db.Use(logger.NewTracingPlugin()); err != nil {
		return nil, nil, fmt.Errorf("register tracing plugin: %w", err)
	}

	// Register GORM metrics plugin for Prometheus query metrics.
	if err := db.Use(dbmetrics.NewMetricsPlugin()); err != nil {
		return nil, nil, fmt.Errorf("register metrics plugin: %w", err)
	}

	// Configure database connection pool.
	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("get underlying sql.DB: %w", err)
	}
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	return db, sqlDB, nil
}

// initRedis initializes the Redis client and instruments it with OpenTelemetry.
func initRedis(cfg *config.Config) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		PoolSize:     100,             // 根据并发量调整，默认 10 在高并发下易成瓶颈
		MinIdleConns: 10,              // 保持最小空闲连接，减少连接建立开销
		MaxRetries:   3,               // 最大重试次数，提升网络抖动下的成功率
		DialTimeout:  5 * time.Second, // 连接超时
		ReadTimeout:  3 * time.Second, // 读超时，防止请求永久阻塞
		WriteTimeout: 3 * time.Second, // 写超时，防止请求永久阻塞
		PoolTimeout:  4 * time.Second, // 连接池超时，等待空闲连接的上限
	})

	// Integrate OpenTelemetry tracing and metrics for Redis
	// This enables distributed tracing spans and Prometheus metrics for all Redis operations
	if err := redisotel.InstrumentTracing(rdb); err != nil {
		logger.Error(context.Background(), "Failed to instrument Redis tracing", zap.Error(err))
	}
	if err := redisotel.InstrumentMetrics(rdb); err != nil {
		logger.Error(context.Background(), "Failed to instrument Redis metrics", zap.Error(err))
	}

	// Verify Redis connection at startup.
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("ping Redis: %w", err)
	}

	return rdb, nil
}
