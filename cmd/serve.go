package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/tracing"
	"github.com/rtc-agent/server/pkg/logger"

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
		logger.Info(context.Background(), "Server log file enabled — writing to "+cfg.Log.ServerLogFile)
	}

	// Init tracing (OpenTelemetry + Jaeger)
	var shutdownTracing func(context.Context) error
	if cfg.Tracing.Enabled {
		var err error
		shutdownTracing, err = tracing.Init(tracing.Config{
			ServiceName: "rtc-agent",
			Endpoint:    cfg.Tracing.Endpoint,
			Insecure:    true, // use unencrypted connection for dev environment
			SampleRate:  cfg.Tracing.SampleRate,
		})
		if err != nil {
			logger.Fatal(context.Background(), "Failed to initialize tracing", zap.Error(err))
		}
		logger.Info(context.Background(), "Tracing enabled",
			zap.String("endpoint", cfg.Tracing.Endpoint),
			zap.Float64("sample_rate", cfg.Tracing.SampleRate),
		)
	}
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
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.NewGormLogger(
			true,                 // ignore ErrRecordNotFound
			200*time.Millisecond, // slow query threshold
		),
	})
	if err != nil {
		logger.Fatal(context.Background(), "Failed to connect database", zap.Error(err))
	}

	// Configure database connection pool.
	sqlDB, err := db.DB()
	if err != nil {
		logger.Fatal(context.Background(), "Failed to get underlying sql.DB", zap.Error(err))
	}
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	// Note: schema migration should use the dedicated `rtc-agent migrate` command, not auto-execute in serve.

	// Init Redis
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	// Verify Redis connection at startup.
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		logger.Fatal(context.Background(), "Failed to connect Redis", zap.Error(err))
	}
	defer func() { _ = rdb.Close() }()

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
	<-quit

	logger.Info(context.Background(), "Shutting down server...")
	srv.Stop()
}
