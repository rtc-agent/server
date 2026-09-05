package svc

import (
	stdcontext "context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/contextx"
	centrifugeplus "github.com/rtc-agent/server/pkg/centrifuge-plus"
	"github.com/rtc-agent/server/pkg/logger"
)

// rpcHandlerInstance 全局 RPC 处理器实例，由 RegisterRPCHandler 设置。
// 使用 sync.RWMutex 保护并发访问。
var (
	rpcHandlerMu       sync.RWMutex
	rpcHandlerInstance RPCHandler
)

// RPCHandler RPC 处理接口，用于打断 svc 对 rpchandler 的反向依赖。
type RPCHandler interface {
	HandleRPC(ctx stdcontext.Context, method string, data []byte) ([]byte, error)
}

// AssembleDualBroker 组装 DualBroker 的核心逻辑：创建 Redis shard → 构建 DualBroker → 配置事件处理。
// 供 Wire 路径（provideDualBroker）和非 Wire 路径（servicecontext.go）共享。
func AssembleDualBroker(node *centrifuge.Node, cfg *config.Config, historyStore centrifugeplus.HistoryStore, jwtSigner *auth.JWTSigner) (*centrifugeplus.DualBroker, error) {
	redisShard, err := centrifuge.NewRedisShard(node, centrifuge.RedisShardConfig{
		Address: cfg.Redis.Addr,
	})
	if err != nil {
		return nil, fmt.Errorf("create redis shard: %w", err)
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
		return nil, fmt.Errorf("create broker: %w", err)
	}

	if err := setupCentrifuge(node, broker, jwtSigner, cfg.Server.RPCTimeout); err != nil {
		return nil, fmt.Errorf("setup centrifuge: %w", err)
	}

	return broker, nil
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

// setupCentrifuge 配置 centrifuge.Node 的事件处理（JWT 鉴权、频道订阅校验）
func setupCentrifuge(
	node *centrifuge.Node, broker *centrifugeplus.DualBroker,
	signer *auth.JWTSigner, rpcTimeout time.Duration,
) error {
	node.SetBroker(broker)
	node.OnConnecting(createOnConnectingHandler(signer))
	node.OnConnect(createOnConnectHandler(broker, rpcTimeout))

	if err := node.Run(); err != nil {
		return fmt.Errorf("run node: %w", err)
	}

	logger.Info(stdcontext.Background(), "centrifuge node started")

	return nil
}

// createOnConnectingHandler 返回 OnConnecting 回调：JWT 验证 → 提取身份 → 写入 Credentials。
func createOnConnectingHandler(signer *auth.JWTSigner) func(stdcontext.Context, centrifuge.ConnectEvent) (centrifuge.ConnectReply, error) {
	return func(ctx stdcontext.Context, e centrifuge.ConnectEvent) (centrifuge.ConnectReply, error) {
		claims, err := signer.ParseAccessToken(e.Token)
		if err != nil {
			logger.Warn(ctx, "JWT 验证失败", zap.Error(err))
			return centrifuge.ConnectReply{}, centrifuge.DisconnectInvalidToken
		}

		ci := &clientInfo{
			UserID:   claims.UserID,
			DeviceID: claims.DeviceID,
		}
		info, _ := json.Marshal(ci)
		return centrifuge.ConnectReply{
			Credentials: &centrifuge.Credentials{
				UserID: claims.UserID.String(),
				Info:   info,
			},
			Data: info,
		}, nil
	}
}

// createOnConnectHandler 返回 OnConnect 回调：订阅校验 + History + RPC 处理。
func createOnConnectHandler(broker *centrifugeplus.DualBroker, rpcTimeout time.Duration) func(*centrifuge.Client) {
	return func(client *centrifuge.Client) {
		userIDStr := client.UserID()
		userID, _ := uuid.Parse(userIDStr)
		ci := parseClientInfo(client.Info())

		setupSubscribeHandler(client, broker)
		setupHistoryHandler(client)
		setupRPCHandler(client, userID, ci.DeviceID, rpcTimeout)

		logger.Info(stdcontext.Background(), "[Centrifuge] client connected",
			zap.String("client_id", client.ID()),
			zap.String("user_id", userID.String()),
		)
	}
}

// setupSubscribeHandler 注册频道订阅回调：校验归属、注册频道类型、启用 recovery。
func setupSubscribeHandler(client *centrifuge.Client, broker *centrifugeplus.DualBroker) {
	client.OnSubscribe(func(e centrifuge.SubscribeEvent, cb centrifuge.SubscribeCallback) {
		ch := e.Channel

		// 用户频道校验：userID 必须等于当前连接的 UserID
		if ownerIDStr, ok := channel.ParseUser(ch); ok {
			if ownerIDStr != client.UserID() {
				cb(centrifuge.SubscribeReply{}, centrifuge.ErrorPermissionDenied)
				return
			}
		}

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
}

// setupHistoryHandler 注册 History 命令处理：返回空 Result 使 centrifuge 回退到 node.History()。
func setupHistoryHandler(client *centrifuge.Client) {
	client.OnHistory(func(e centrifuge.HistoryEvent, cb centrifuge.HistoryCallback) {
		cb(centrifuge.HistoryReply{}, nil)
	})
}

// setupRPCHandler 注册 RPC 处理回调：注入身份 context → 分发到全局 RPCHandler。
func setupRPCHandler(client *centrifuge.Client, userID uuid.UUID, deviceID string, rpcTimeout time.Duration) {
	client.OnRPC(func(e centrifuge.RPCEvent, cb centrifuge.RPCCallback) {
		ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), rpcTimeout)
		defer cancel()

		ctx = contextx.WithClientInfo(ctx, userID, deviceID)

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
			cb(centrifuge.RPCReply{}, &centrifuge.Error{
				Code:    500,
				Message: err.Error(),
			})
			return
		}
		cb(centrifuge.RPCReply{Data: resp}, nil)
	})
}

// RegisterRPCHandler 注册 RPC 处理器。
// 由 server 层在创建 RPCHandler 后调用，供已建立的连接在 OnRPC 回调中分发。
func RegisterRPCHandler(handler RPCHandler) {
	rpcHandlerMu.Lock()
	rpcHandlerInstance = handler
	rpcHandlerMu.Unlock()
}
