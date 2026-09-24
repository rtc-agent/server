package svc

import (
	stdcontext "context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/contextx"
	centrifugeplus "github.com/rtc-agent/server/pkg/centrifuge-plus"
	"github.com/rtc-agent/server/pkg/logger"
)

// rpcHandlerInstance is the global RPC handler instance, set by RegisterRPCHandler.
// Access is protected by sync.RWMutex.
var (
	rpcHandlerMu       sync.RWMutex
	rpcHandlerInstance RPCHandler
)

// RPCHandler is the RPC processing interface, used to break the reverse
// dependency from svc to the rpchandler package.
type RPCHandler interface {
	HandleRPC(ctx stdcontext.Context, method string, data []byte) ([]byte, error)
}

// AssembleDualBroker assembles the DualBroker core logic: create Redis shard,
// build DualBroker, and configure event handlers. Shared between the Wire
// path (provideDualBroker) and the non-Wire path (servicecontext.go).
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
			Logger:        &centrifugeLogger{ctx: stdcontext.Background()},
			Tracing:       centrifugeplus.TracingConfig{Enabled: true, Provider: otel.GetTracerProvider()},
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

// clientInfo holds additional information stored in Credentials.Info.
type clientInfo struct {
	UserID   uuid.UUID `json:"user_id"`
	DeviceID string    `json:"device_id"`
}

// parseClientInfo parses clientInfo from client.Info() JSON.
// On parse failure, logs a warning and returns a zero value, ensuring
// OnConnect does not panic due to corrupted info.
func parseClientInfo(info []byte) *clientInfo {
	ci := &clientInfo{}
	if len(info) > 0 {
		if err := json.Unmarshal(info, ci); err != nil {
			logger.Warn(stdcontext.Background(), "[Centrifuge] failed to parse client info, using zero value",
				zap.Error(err),
				zap.String("info_preview", previewToken(string(info))),
			)
		}
	}
	return ci
}

// setupCentrifuge configures the centrifuge.Node event handlers (JWT
// verification, channel subscription validation).
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

// centrifugeLogger adapts the zap logger to the centrifuge-plus Logger interface.
type centrifugeLogger struct {
	ctx stdcontext.Context
}

func (l *centrifugeLogger) Info(msg string, args ...any) {
	logger.Info(l.ctx, msg, zap.Any("args", args))
}

func (l *centrifugeLogger) Warn(msg string, args ...any) {
	logger.Warn(l.ctx, msg, zap.Any("args", args))
}

func (l *centrifugeLogger) Error(msg string, args ...any) {
	logger.Error(l.ctx, msg, zap.Any("args", args))
}

// createOnConnectingHandler returns the OnConnecting callback: JWT verification
// -> extract identity -> write to Credentials.
func createOnConnectingHandler(signer *auth.JWTSigner) func(stdcontext.Context, centrifuge.ConnectEvent) (centrifuge.ConnectReply, error) {
	return func(ctx stdcontext.Context, e centrifuge.ConnectEvent) (centrifuge.ConnectReply, error) {
		logger.Info(ctx, "[Centrifuge] OnConnecting started",
			zap.String("token_length", fmt.Sprintf("%d", len(e.Token))),
			zap.Bool("has_token", len(e.Token) > 0),
		)

		claims, err := signer.ParseAccessToken(e.Token)
		if err != nil {
			logger.Error(ctx, "[Centrifuge] JWT verification failed, rejecting connection",
				zap.Error(err),
				zap.String("token_preview", previewToken(e.Token)),
			)
			return centrifuge.ConnectReply{}, centrifuge.DisconnectInvalidToken
		}

		ci := &clientInfo{
			UserID:   claims.UserID,
			DeviceID: claims.DeviceID,
		}
		info, _ := json.Marshal(ci)

		logger.Info(ctx, "[Centrifuge] OnConnecting succeeded",
			zap.String("user_id", claims.UserID.String()),
			zap.String("device_id", claims.DeviceID),
		)

		return centrifuge.ConnectReply{
			Credentials: &centrifuge.Credentials{
				UserID: claims.UserID.String(),
				Info:   info,
			},
			Data: info,
		}, nil
	}
}

// previewToken truncates a token to the first 20 characters for logging
// (avoids recording the full token).
func previewToken(token string) string {
	if len(token) == 0 {
		return "<empty>"
	}
	if len(token) <= 20 {
		return token
	}
	return token[:20] + "..."
}

// createOnConnectHandler returns the OnConnect callback: subscription
// validation + History + RPC handling.
func createOnConnectHandler(broker *centrifugeplus.DualBroker, rpcTimeout time.Duration) func(*centrifuge.Client) {
	return func(client *centrifuge.Client) {
		// Panic recovery to capture and log any unhandled errors.
		defer func() {
			if r := recover(); r != nil {
				logger.Error(stdcontext.Background(), "[Centrifuge] OnConnect panic",
					zap.Any("panic", r),
					zap.String("client_id", client.ID()),
					zap.String("user_id", client.UserID()),
					zap.Stack("stack"),
				)
			}
		}()

		logger.Info(stdcontext.Background(), "[Centrifuge] OnConnect started",
			zap.String("client_id", client.ID()),
			zap.String("user_id", client.UserID()),
		)

		userIDStr := client.UserID()
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			logger.Error(stdcontext.Background(), "[Centrifuge] Failed to parse user_id",
				zap.Error(err),
				zap.String("user_id_str", userIDStr),
				zap.String("client_id", client.ID()),
			)
			// Disconnect the client.
			client.Disconnect(centrifuge.DisconnectBadRequest)
			return
		}

		ci := parseClientInfo(client.Info())

		logger.Info(stdcontext.Background(), "[Centrifuge] Setting up handlers",
			zap.String("client_id", client.ID()),
			zap.String("user_id", userID.String()),
			zap.String("device_id", ci.DeviceID),
		)

		// Create a long-lived span for this client connection.
		// All RPC calls will be child spans of this connection span.
		// Use client.Context() (which carries the HTTP/WebSocket upgrade request's trace)
		// as parent, so the entire centrifuge trace tree is linked to the original request.
		tracer := otel.Tracer("centrifuge")
		clientCtx, clientSpan := tracer.Start(client.Context(), "Centrifuge Client",
			trace.WithAttributes(
				attribute.String("client.id", client.ID()),
				attribute.String("user.id", userID.String()),
				attribute.String("device.id", ci.DeviceID),
			),
		)

		setupSubscribeHandler(client, broker, clientCtx)
		setupHistoryHandler(client, clientCtx)
		setupRPCHandler(client, userID, ci.DeviceID, rpcTimeout, clientCtx)
		setupDisconnectHandler(client, userID, ci.DeviceID, clientSpan)

		logger.Info(stdcontext.Background(), "[Centrifuge] client connected",
			zap.String("client_id", client.ID()),
			zap.String("user_id", userID.String()),
		)
	}
}

// setupDisconnectHandler registers the disconnect callback: ends the client span
// and records slow disconnects as errors so they appear in Jaeger.
func setupDisconnectHandler(client *centrifuge.Client, userID uuid.UUID, deviceID string, clientSpan trace.Span) {
	client.OnDisconnect(func(e centrifuge.DisconnectEvent) {
		reason := e.Reason
		code := e.Code

		if reason == "slow" {
			clientSpan.RecordError(fmt.Errorf("client disconnected: slow (code %d)", code))
			clientSpan.SetStatus(codes.Error, "slow disconnect")

			logger.Error(stdcontext.Background(), "[Centrifuge] client disconnected: slow",
				zap.String("client_id", client.ID()),
				zap.String("user_id", userID.String()),
				zap.String("device_id", deviceID),
				zap.Uint32("code", code),
			)
		} else {
			logger.Info(stdcontext.Background(), "[Centrifuge] client disconnected",
				zap.String("client_id", client.ID()),
				zap.String("user_id", userID.String()),
				zap.String("reason", reason),
				zap.Uint32("code", code),
			)
		}

		// End the long-lived client span.
		clientSpan.End()
	})
}

// setupSubscribeHandler registers the channel subscription callback:
// validates ownership, registers channel type, enables recovery.
func setupSubscribeHandler(client *centrifuge.Client, broker *centrifugeplus.DualBroker, clientCtx stdcontext.Context) {
	client.OnSubscribe(func(e centrifuge.SubscribeEvent, cb centrifuge.SubscribeCallback) {
		// Create trace span for subscribe operation (as child of client span)
		tracer := otel.Tracer("centrifuge")
		ctx, span := tracer.Start(clientCtx, "Centrifuge Subscribe",
			trace.WithAttributes(
				attribute.String("channel", e.Channel),
			),
		)
		defer span.End()

		_ = ctx // ctx available for future use if needed

		ch := e.Channel

		// User channel validation: userID must match the connected user.
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

// setupHistoryHandler registers the History command handler: returns an
// empty Result so centrifuge falls back to node.History().
func setupHistoryHandler(client *centrifuge.Client, clientCtx stdcontext.Context) {
	client.OnHistory(func(e centrifuge.HistoryEvent, cb centrifuge.HistoryCallback) {
		// Create trace span for history operation (as child of client span)
		tracer := otel.Tracer("centrifuge")
		ctx, span := tracer.Start(clientCtx, "Centrifuge History",
			trace.WithAttributes(
				attribute.String("channel", e.Channel),
			),
		)
		defer span.End()

		_ = ctx // ctx available for future use if needed

		cb(centrifuge.HistoryReply{}, nil)
	})
}

// setupRPCHandler registers the RPC handler callback: injects identity
// context and dispatches to the global RPCHandler.
func setupRPCHandler(client *centrifuge.Client, userID uuid.UUID, deviceID string, rpcTimeout time.Duration, clientCtx stdcontext.Context) {
	client.OnRPC(func(e centrifuge.RPCEvent, cb centrifuge.RPCCallback) {
		logger.Info(stdcontext.Background(), "[Centrifuge] RPC called",
			zap.String("method", e.Method),
			zap.String("client_id", client.ID()),
		)

		ctx, cancel := stdcontext.WithTimeout(clientCtx, rpcTimeout)
		defer cancel()

		// Create trace span for RPC request (as child of client span)
		tracer := otel.Tracer("rpc")
		ctx, span := tracer.Start(ctx, "RPC "+e.Method,
			trace.WithAttributes(
				attribute.String("rpc.method", e.Method),
				attribute.String("user.id", userID.String()),
				attribute.String("device.id", deviceID),
			),
		)
		defer span.End()

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
			// Only forward APIError (sanitized, safe errors); other errors
			// return a generic message to prevent leaking internal details
			// (database statements, connection info) to the client.
			if apiErr, ok := extractAPIError(err); ok {
				span.RecordError(err)
				cb(centrifuge.RPCReply{}, &centrifuge.Error{
					Code:    500,
					Message: apiErr,
				})
			} else {
				logger.Error(ctx, "RPC handler returned non-API error",
					zap.String("method", e.Method),
					zap.Error(err))
				span.RecordError(err)
				cb(centrifuge.RPCReply{}, &centrifuge.Error{
					Code:    500,
					Message: "internal error",
				})
			}
			return
		}
		cb(centrifuge.RPCReply{Data: resp}, nil)
	})
}

// extractAPIError attempts to extract a safe, client-visible error message
// from an error. It checks whether the error implements a struct with
// Code/Message fields.
func extractAPIError(err error) (string, bool) {
	// rpchandler.APIError has Code and Message fields.
	// Use structural typing to avoid directly importing the rpchandler package.
	type safeError interface {
		Error() string
		SafeMessage() string
	}
	if se, ok := err.(safeError); ok {
		return se.SafeMessage(), true
	}
	return "", false
}

// RegisterRPCHandler registers the RPC handler.
// Called by the server layer after creating the RPCHandler, so that
// established connections can dispatch via OnRPC callbacks.
func RegisterRPCHandler(handler RPCHandler) {
	rpcHandlerMu.Lock()
	rpcHandlerInstance = handler
	rpcHandlerMu.Unlock()
}
