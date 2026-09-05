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
)

// ServiceContext 服务上下文，用于依赖注入
// 仅持有 repos 和基础设施（DB、Redis、Centrifuge），不包含业务逻辑
type ServiceContext struct {
	Config *config.Config
	DB     *gorm.DB
	Redis  redis.UniversalClient

	// Repos
	SessionRepo      repo.SessionRepo
	MessageRepo      repo.MessageRepo
	TurnRepo         repo.TurnRepo
	RtcRepo          repo.RtcRepo
	OAuth2UserRepo   repo.OAuth2UserRepo
	DeviceRepo       repo.DeviceRepo
	RefreshTokenRepo repo.RefreshTokenRepo

	// 基础设施
	UpdatePublisher *updates.UpdatePublisher
	CentrifugeNode  *centrifuge.Node
	Broker          *centrifugeplus.DualBroker
	JWTSigner       *auth.JWTSigner
}

// NewServiceContext 创建服务上下文
// 保留原有签名以兼容非 Wire 调用方。内部委托给 NewServiceContextWithDeps。
func NewServiceContext(cfg *config.Config, db *gorm.DB, rdb redis.UniversalClient) *ServiceContext {
	// 创建 repos
	sessionRepo := repo.NewSessionRepo(db)
	messageRepo := repo.NewMessageRepo(db)
	turnRepo := repo.NewTurnRepo(db)
	rtcRepo := repo.NewRtcRepo(db)
	oauth2UserRepo := repo.NewOAuth2UserRepo(db)
	deviceRepo := repo.NewDeviceRepo(db)
	refreshTokenRepo := repo.NewRefreshTokenRepo(db)

	// 创建 UpdatePublisher（需要先创建 repos）
	updatePublisher := updates.NewUpdatePublisher(db, rdb, sessionRepo, messageRepo, turnRepo, rtcRepo)

	jwtSigner, err := auth.NewJWTSigner(
		cfg.Auth.JWTSecret,
		time.Duration(cfg.Auth.AccessTokenTTLSeconds)*time.Second,
	)
	if err != nil {
		logger.Fatal(context.Background(), "初始化 JWT 签名器失败", zap.Error(err))
	}

	node, dualBroker, err := newCentrifugeBroker(cfg, updatePublisher, jwtSigner)
	if err != nil {
		logger.Fatal(context.Background(), "new centrifuge", zap.Error(err))
	}

	// 注入 broker 到 UpdatePublisher（解决循环依赖）
	updatePublisher.SetBroker(dualBroker)

	return &ServiceContext{
		Config:           cfg,
		DB:               db,
		Redis:            rdb,
		SessionRepo:      sessionRepo,
		MessageRepo:      messageRepo,
		TurnRepo:         turnRepo,
		RtcRepo:          rtcRepo,
		OAuth2UserRepo:   oauth2UserRepo,
		DeviceRepo:       deviceRepo,
		RefreshTokenRepo: refreshTokenRepo,
		UpdatePublisher:  updatePublisher,
		CentrifugeNode:   node,
		Broker:           dualBroker,
		JWTSigner:        jwtSigner,
	}
}

// NewServiceContextWithDeps 创建服务上下文（Wire 兼容版本）。
// 所有依赖由调用方提供，便于 Wire 注入。
func NewServiceContextWithDeps(
	cfg *config.Config,
	db *gorm.DB,
	rdb redis.UniversalClient,
	sessionRepo repo.SessionRepo,
	messageRepo repo.MessageRepo,
	turnRepo repo.TurnRepo,
	rtcRepo repo.RtcRepo,
	oauth2UserRepo repo.OAuth2UserRepo,
	deviceRepo repo.DeviceRepo,
	refreshTokenRepo repo.RefreshTokenRepo,
	updatePublisher *updates.UpdatePublisher,
	node *centrifuge.Node,
	broker *centrifugeplus.DualBroker,
	jwtSigner *auth.JWTSigner,
) *ServiceContext {
	// 注入 broker 到 UpdatePublisher（解决循环依赖）
	updatePublisher.SetBroker(broker)

	return &ServiceContext{
		Config:           cfg,
		DB:               db,
		Redis:            rdb,
		SessionRepo:      sessionRepo,
		MessageRepo:      messageRepo,
		TurnRepo:         turnRepo,
		RtcRepo:          rtcRepo,
		OAuth2UserRepo:   oauth2UserRepo,
		DeviceRepo:       deviceRepo,
		RefreshTokenRepo: refreshTokenRepo,
		UpdatePublisher:  updatePublisher,
		CentrifugeNode:   node,
		Broker:           broker,
		JWTSigner:        jwtSigner,
	}
}
