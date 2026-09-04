package svc

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/rtc-agent/server/internal/auth"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/config"
	"github.com/rtc-agent/server/internal/contextx"
	centrifugeplus "github.com/rtc-agent/server/pkg/centrifuge-plus"
	"github.com/rtc-agent/server/pkg/logger"
	"sync"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
)

// rpcHandlerInstance 全局 RPC 处理器实例，由 RegisterRPCHandler 设置。
// 使用 sync.RWMutex 保护并发访问。
var (
	rpcHandlerMu       sync.RWMutex
	rpcHandlerInstance RPCHandler
)

// RPCHandler RPC 处理接口，用于打断 svc 对 rpchandler 的反向依赖。
type RPCHandler interface {
	HandleRPC(ctx context.Context, method string, data []byte) ([]byte, error)
}

// NewCentrifugeBroker 创建 DualBroker（需要先有 centrifuge.Node 供 RedisShard 使用）。
// 导出供 Wire 使用。
func NewCentrifugeBroker(cfg *config.Config, historyStore centrifugeplus.HistoryStore, jwtSigner *auth.JWTSigner) (*centrifuge.Node, *centrifugeplus.DualBroker, error) {
	return newCentrifugeBroker(cfg, historyStore, jwtSigner)
}

// newCentrifugeBroker 创建 DualBroker（需要先有 centrifuge.Node 供 RedisShard 使用）
func newCentrifugeBroker(cfg *config.Config, historyStore centrifugeplus.HistoryStore, jwtSigner *auth.JWTSigner) (*centrifuge.Node, *centrifugeplus.DualBroker, error) {
	node, err := centrifuge.New(centrifuge.Config{
		LogLevel: centrifuge.LogLevelInfo,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create centrifuge node: %w", err)
	}
	redisShard, err := centrifuge.NewRedisShard(node, centrifuge.RedisShardConfig{
		Address: cfg.Redis.Addr,
	})
	if err != nil {
		return node, nil, fmt.Errorf("create redis shard: %w", err)
	}

	broker, err := centrifugeplus.NewDualBroker(node, centrifugeplus.DualBrokerConfig{
		Live: centrifuge.RedisBrokerConfig{
			Prefix: channel.LivePrefix,
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: centrifugeplus.TopicBrokerConfig{
			Prefix:        channel.TopicPrefix,
			RedisAddr:     cfg.Redis.Addr,
			RedisPassword: cfg.Redis.Password,
			RedisDB:       cfg.Redis.DB,
			HistoryStore:  historyStore,
			// TODO: 注入 Logger（待 centrifuge-plus 支持 Logger 配置）
		},
	})
	if err != nil {
		return node, nil, fmt.Errorf("create broker: %w", err)
	}

	if err := setupCentrifuge(node, broker, jwtSigner, cfg.Server.RPCTimeout); err != nil {
		return node, nil, fmt.Errorf("setup centrifuge: %w", err)
	}

	return node, broker, nil
}

// clientInfo 存储在 Credentials.Info 中的额外信息
type clientInfo struct {
	UserID   uuid.UUID `json:"user_id"`
	DeviceID string    `json:"device_id"`
}

// parseClientInfo 从 client.Info() JSON 解析出 clientInfo。
// 解析失败或为空时返回零值（不报错），保证 OnConnect 不因 info 损坏而 panic。
func parseClientInfo(info []byte) *clientInfo {
	ci := &clientInfo{}
	if len(info) > 0 {
		_ = json.Unmarshal(info, ci)
	}
	return ci
}

// SetupCentrifuge 配置 centrifuge.Node 的事件处理（JWT 鉴权、频道订阅校验）。
// 导出供 Wire 使用。
func SetupCentrifuge(
	node *centrifuge.Node, broker *centrifugeplus.DualBroker,
	signer *auth.JWTSigner, rpcTimeout time.Duration,
) error {
	return setupCentrifuge(node, broker, signer, rpcTimeout)
}

// setupCentrifuge 配置 centrifuge.Node 的事件处理（JWT 鉴权、频道订阅校验）
func setupCentrifuge(
	node *centrifuge.Node, broker *centrifugeplus.DualBroker,
	signer *auth.JWTSigner, rpcTimeout time.Duration,
) error {
	node.SetBroker(broker)

	// OnConnecting: JWT 验证 → 提取 userID/deviceID → 写入 Credentials.Info
	node.OnConnecting(func(ctx context.Context, e centrifuge.ConnectEvent) (centrifuge.ConnectReply, error) {
		// JWT 验证
		claims, err := signer.ParseAccessToken(e.Token)
		if err != nil {
			logger.Warn("JWT 验证失败: %v", err)
			return centrifuge.ConnectReply{}, centrifuge.DisconnectInvalidToken
		}

		userID := claims.UserID
		deviceID := claims.DeviceID

		ci := &clientInfo{
			UserID:   userID,
			DeviceID: deviceID,
		}
		info, _ := json.Marshal(ci)
		return centrifuge.ConnectReply{
			Credentials: &centrifuge.Credentials{
				UserID: userID.String(),
				Info:   info,
			},
			Data: info,
		}, nil
	})

	// OnConnect: 连接建立后的处理（订阅校验 + RPC 回调注册）
	// 合并为单个 handler，避免重复注册和 clientInfo 解析
	node.OnConnect(func(client *centrifuge.Client) {
		userIDStr := client.UserID()
		userID, _ := uuid.Parse(userIDStr)

		// 从 client.Info() 解析 deviceID（统一解析逻辑）
		ci := parseClientInfo(client.Info())

		// 订阅时校验频道归属
		client.OnSubscribe(func(e centrifuge.SubscribeEvent, cb centrifuge.SubscribeCallback) {
			ch := e.Channel

			// 用户频道校验（支持 topic: 和 live: 前缀）
			// 频道格式：{prefix}:u={userID}
			// 校验：userID 必须等于当前连接的 UserID
			if ownerIDStr, ok := channel.ParseUser(ch); ok {
				if ownerIDStr != client.UserID() {
					cb(centrifuge.SubscribeReply{}, centrifuge.ErrorPermissionDenied)
					return
				}
			}

			// 注册频道类型并启用 recovery
			if channel.IsTopic(ch) {
				broker.RegisterChannelType(ch, centrifugeplus.Topic)
				cb(centrifuge.SubscribeReply{
					Options: centrifuge.SubscribeOptions{
						EnableRecovery: true,
					},
				}, nil)
				return
			}
			if channel.IsLive(ch) {
				broker.RegisterChannelType(ch, centrifugeplus.Live)
			}
			cb(centrifuge.SubscribeReply{}, nil)
		})

		// 注册 History 命令处理：返回空 Result 使 centrifuge 回退到 node.History()
		client.OnHistory(func(e centrifuge.HistoryEvent, cb centrifuge.HistoryCallback) {
			cb(centrifuge.HistoryReply{}, nil)
		})

		// RPC 处理回调
		client.OnRPC(func(e centrifuge.RPCEvent, cb centrifuge.RPCCallback) {
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			defer cancel()

			// 注入 userID/deviceID 到 context
			ctx = contextx.WithClientInfo(ctx, userID, ci.DeviceID)

			rpcHandlerMu.RLock()
			handler := rpcHandlerInstance
			rpcHandlerMu.RUnlock()

			if handler == nil {
				cb(centrifuge.RPCReply{}, &centrifuge.Error{
					Code:    500,
					Message: "RPC handler not registered",
				})
				return
			}

			resp, err := handler.HandleRPC(ctx, e.Method, e.Data)
			if err != nil {
				// 保留错误信息，否则 Centrifuge 会返回 "internal server error"
				cb(centrifuge.RPCReply{}, &centrifuge.Error{
					Code:    500,
					Message: err.Error(),
				})
				return
			}
			cb(centrifuge.RPCReply{Data: resp}, nil)
		})

		logger.Info("[Centrifuge] client connected: client_id=%s user_id=%s",
			client.ID(), userID,
		)
	})

	if err := node.Run(); err != nil {
		return fmt.Errorf("run node: %w", err)
	}

	logger.Info("centrifuge node started")

	return nil
}

// RegisterRPCHandler 注册 RPC 处理器。
// 由 server 层在创建 RPCHandler 后调用，供已建立的连接在 OnRPC 回调中分发。
func RegisterRPCHandler(handler RPCHandler) {
	rpcHandlerMu.Lock()
	rpcHandlerInstance = handler
	rpcHandlerMu.Unlock()
}
