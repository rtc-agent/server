package cmd

import (
	"context"
	"database/sql"
	"fmt"
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

	// Init logger
	logger.Init(cfg.Log.Level, cfg.Log.ServerLogFile)
	defer logger.Sync()

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
		if err := srv.Start(); err != nil {
			logger.Error(context.Background(), "Server failed", zap.Error(err))
			// Signal the main goroutine to exit.
			// Using signal.Notify + kill self preserves the deferred cleanup
			// (logger.Sync, rdb.Close) that logger.Fatal would bypass.
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
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

	// Shutdown sequence (in reverse order of startup):
	// 1. Stop HTTP server (stops accepting new connections)
	// 2. Stop Server (stops workers, centrifuge, etc.)
	// Note: database and Redis connections are closed by deferred functions.

	// Stop the main server (handles workers, centrifuge, asynq, etc.)
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

	shutdownTracing, err := tracing.Init(tracing.Config{
		ServiceName: "rtc-agent",
		Endpoint:    cfg.Tracing.Endpoint,
		Insecure:    true, // use unencrypted connection for dev environment
		SampleRate:  cfg.Tracing.SampleRate,
	})
	if err != nil {
		logger.Fatal(context.Background(), "Failed to initialize tracing", zap.Error(err))
	}
	logger.Info(context.Background(), "Tracing enabled")
	logger.Debug(context.Background(), "Tracing configuration",
		zap.String("endpoint", cfg.Tracing.Endpoint),
		zap.Float64("sample_rate", cfg.Tracing.SampleRate),
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
