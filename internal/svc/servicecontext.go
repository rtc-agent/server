package svc

import (
	"context"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	centrifugeplus "github.com/rtc-agent/server/pkg/centrifuge-plus"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/memory"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// ServiceContext holds the service dependencies for dependency injection.
// It contains only repos and infrastructure (DB, Redis, Centrifuge) — no business logic.
//
//nolint:dupl // struct fields mirror constructor params — unavoidable Go DI pattern
type ServiceContext struct {
	Config *config.Config
	DB     *gorm.DB
	Redis  redis.UniversalClient

	// Repos
	SessionRepo         repo.SessionRepo
	MessageRepo         repo.MessageRepo
	TurnRepo            repo.TurnRepo
	RtcRepo             repo.RtcRepo
	GoalRepo            repo.GoalRepo
	OAuth2UserRepo      repo.OAuth2UserRepo
	DeviceRepo          repo.DeviceRepo
	RefreshTokenRepo    repo.RefreshTokenRepo
	SessionMemoryRepo   repo.SessionMemoryRepo
	UserMemoryRepo      repo.UserMemoryRepo
	ScriptExecutionRepo repo.ScriptExecutionRepo
	MemoryRepo          memory.Repository // Phase 2: unified Memory storage
	LoopRepo            repo.LoopRepo

	// Infrastructure
	UpdatePublisher *updates.UpdatePublisher
	CentrifugeNode  *centrifuge.Node
	Broker          *centrifugeplus.DualBroker
	JWTSigner       *auth.JWTSigner
}

// NewServiceContext creates a ServiceContext.
// Retains the original signature for non-Wire callers. Internally delegates to NewServiceContextWithDeps.
func NewServiceContext(cfg *config.Config, db *gorm.DB, rdb redis.UniversalClient) *ServiceContext {
	sessionRepo := repo.NewSessionRepo(db)
	messageRepo := repo.NewMessageRepo(db)
	turnRepo := repo.NewTurnRepo(db)
	rtcRepo := repo.NewRtcRepo(db)
	goalRepo := repo.NewGoalRepo(db)
	oauth2UserRepo := repo.NewOAuth2UserRepo(db)
	deviceRepo := repo.NewDeviceRepo(db)
	refreshTokenRepo := repo.NewRefreshTokenRepo(db)
	sessionMemoryRepo := repo.NewSessionMemoryRepo(db)
	userMemoryRepo := repo.NewUserMemoryRepo(db)
	scriptExecutionRepo := repo.NewScriptExecutionRepo(db)
	memoryRepo := repo.NewMemoryRepo(db)
	loopRepo := repo.NewLoopRepo(db)

	updatePublisher := updates.NewUpdatePublisher(db, rdb, sessionRepo, messageRepo, turnRepo, rtcRepo)

	jwtSigner, err := auth.NewJWTSigner(
		cfg.Auth.JWTSecret,
		time.Duration(cfg.Auth.AccessTokenTTLSeconds)*time.Second,
	)
	if err != nil {
		logger.Fatal(context.Background(), "init JWT signer", zap.Error(err))
	}

	node, err := centrifuge.New(centrifuge.Config{
		LogLevel:   centrifuge.LogLevelInfo,
		LogHandler: createCentrifugeLogHandler(),
	})
	if err != nil {
		logger.Fatal(context.Background(), "create centrifuge node", zap.Error(err))
	}
	dualBroker, err := AssembleDualBroker(node, cfg, updatePublisher, jwtSigner)
	if err != nil {
		if shutdownErr := node.Shutdown(context.Background()); shutdownErr != nil {
			logger.Error(context.Background(), "centrifuge node shutdown after broker assembly failure", zap.Error(shutdownErr))
		}
		logger.Fatal(context.Background(), "assemble dual broker", zap.Error(err))
	}

	return NewServiceContextWithDeps(cfg, db, rdb,
		sessionRepo, messageRepo, turnRepo, rtcRepo,
		goalRepo, oauth2UserRepo, deviceRepo, refreshTokenRepo,
		sessionMemoryRepo, userMemoryRepo, scriptExecutionRepo, memoryRepo, loopRepo,
		updatePublisher, node, dualBroker, jwtSigner)
}

// NewServiceContextWithDeps creates a ServiceContext (Wire-compatible).
// All dependencies are provided by the caller for Wire injection.
//
//nolint:dupl // constructor params mirror struct fields — unavoidable Go pattern
func NewServiceContextWithDeps(
	cfg *config.Config,
	db *gorm.DB,
	rdb redis.UniversalClient,
	sessionRepo repo.SessionRepo,
	messageRepo repo.MessageRepo,
	turnRepo repo.TurnRepo,
	rtcRepo repo.RtcRepo,
	goalRepo repo.GoalRepo,
	oauth2UserRepo repo.OAuth2UserRepo,
	deviceRepo repo.DeviceRepo,
	refreshTokenRepo repo.RefreshTokenRepo,
	sessionMemoryRepo repo.SessionMemoryRepo,
	userMemoryRepo repo.UserMemoryRepo,
	scriptExecutionRepo repo.ScriptExecutionRepo,
	memoryRepo memory.Repository,
	loopRepo repo.LoopRepo,
	updatePublisher *updates.UpdatePublisher,
	node *centrifuge.Node,
	broker *centrifugeplus.DualBroker,
	jwtSigner *auth.JWTSigner,
) *ServiceContext {
	updatePublisher.SetBroker(broker)

	configureUpdatePublisher(updatePublisher, cfg)
	initTokenCounter(cfg)

	return &ServiceContext{
		Config:              cfg,
		DB:                  db,
		Redis:               rdb,
		SessionRepo:         sessionRepo,
		MessageRepo:         messageRepo,
		TurnRepo:            turnRepo,
		RtcRepo:             rtcRepo,
		GoalRepo:            goalRepo,
		OAuth2UserRepo:      oauth2UserRepo,
		DeviceRepo:          deviceRepo,
		RefreshTokenRepo:    refreshTokenRepo,
		SessionMemoryRepo:   sessionMemoryRepo,
		UserMemoryRepo:      userMemoryRepo,
		ScriptExecutionRepo: scriptExecutionRepo,
		MemoryRepo:          memoryRepo,
		LoopRepo:            loopRepo,
		UpdatePublisher:     updatePublisher,
		CentrifugeNode:      node,
		Broker:              broker,
		JWTSigner:           jwtSigner,
	}
}

// configureUpdatePublisher sets compression trigger thresholds (with 80% fallback protection).
func configureUpdatePublisher(u *updates.UpdatePublisher, cfg *config.Config) {
	contextLimit := cfg.Worker.ContextTokensLimit
	if contextLimit <= 0 {
		contextLimit = 25000
	}
	compactBuffer := cfg.Worker.AutoCompactBufferTokens
	if compactBuffer <= 0 {
		compactBuffer = 13000
	}
	threshold := contextLimit - compactBuffer
	if threshold <= 0 {
		// Fallback to 80% of contextLimit when configuration is invalid
		// This prevents threshold=1 which would cause compression on every turn
		threshold = int(float64(contextLimit) * 0.8)
		logger.Warn(context.Background(), "servicecontext.threshold_fallback",
			zap.Int("context_limit", contextLimit),
			zap.Int("compact_buffer", compactBuffer),
			zap.Int("fallback_threshold", threshold))
	}
	u.SetCompressionThreshold(int64(threshold))
}

// initTokenCounter initialises the global TokenCounter.
func initTokenCounter(cfg *config.Config) {
	tc := turnagent.NewTokenCounter(cfg.Worker.TokenCounterMode)
	turnagent.SetGlobalTokenCounter(tc)
	logger.Info(context.Background(), "servicecontext.token_counter_initialized",
		zap.String("mode", cfg.Worker.TokenCounterMode))
}

// createCentrifugeLogHandler creates a Centrifuge log handler that forwards logs to the zap logger.
func createCentrifugeLogHandler() centrifuge.LogHandler {
	return func(entry centrifuge.LogEntry) {
		ctx := context.Background()
		fields := make([]zap.Field, 0, len(entry.Fields)+1)

		// Add Centrifuge fields
		for k, v := range entry.Fields {
			fields = append(fields, zap.Any(k, v))
		}

		// Add error field (if present)
		if entry.Error != nil {
			fields = append(fields, zap.Error(entry.Error))
		}

		// Map Centrifuge log levels to zap log levels
		switch entry.Level {
		case centrifuge.LogLevelTrace, centrifuge.LogLevelDebug:
			logger.Debug(ctx, "[centrifuge] "+entry.Message, fields...)
		case centrifuge.LogLevelInfo:
			logger.Info(ctx, "[centrifuge] "+entry.Message, fields...)
		case centrifuge.LogLevelWarn:
			logger.Warn(ctx, "[centrifuge] "+entry.Message, fields...)
		case centrifuge.LogLevelError:
			logger.Error(ctx, "[centrifuge] "+entry.Message, fields...)
		default:
			logger.Info(ctx, "[centrifuge] "+entry.Message, fields...)
		}
	}
}
