package rpchandler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// Dependencies RPC Handler 所需的依赖
type Dependencies struct {
	Deps        *usecase.Dependencies
	SessionRepo repo.SessionRepo
	Queue       *rtcqueue.Queue // rtc-queue for publishing/cancelling work items
	API         config.APIConfig
}

// Handler RPC 处理器
type Handler struct {
	deps   *Dependencies
	routes map[protocol.RpcMethod]routeHandler
}

// routeHandler 单条路由的处理函数
type routeHandler func(ctx context.Context, data []byte) (any, error)

// NewHandler 创建 RPC 处理器
func NewHandler(deps *Dependencies) *Handler {
	h := &Handler{deps: deps}
	h.registerRoutes()
	return h
}

// registerRoutes 注册所有 RPC 路由。
// 新增 RPC 只需在此处添加一行，无需修改 HandleRPC。
func (h *Handler) registerRoutes() {
	h.routes = map[protocol.RpcMethod]routeHandler{
		// Session
		protocol.MethodSessionList:   dispatch(h, h.ListSessions),
		protocol.MethodSessionGet:    dispatch(h, h.GetSession),
		protocol.MethodSessionClose:  dispatch(h, h.CloseSession),
		protocol.MethodSessionUpdate: dispatch(h, h.UpdateSession),
		protocol.MethodSessionFork:   dispatch(h, h.ForkSession),

		// Message
		protocol.MethodMessageSend: dispatch(h, h.SendMessage),
		protocol.MethodMessageList: dispatch(h, h.MessageList),
		protocol.MethodMessageGet:  dispatch(h, h.MessageGet),

		// Turn
		protocol.MethodTurnList: dispatch(h, h.TurnList),
		protocol.MethodTurnGet:  dispatch(h, h.TurnGet),
		protocol.MethodTurnStop: dispatch(h, h.StopTurn),

		// RTC
		protocol.MethodRtcList:         dispatch(h, h.RtcList),
		protocol.MethodRtcGet:          dispatch(h, h.RtcGet),
		protocol.MethodRtcUpdateStatus: dispatch(h, h.UpdateRtcStatus),
		protocol.MethodRtcSubmitResult: dispatch(h, h.SubmitRtcResult),
	}
}

// dispatch 泛型分发辅助：反序列化请求 → 调用 handler → 返回结果。
// 消除每个 case 中重复的 Unmarshal 样板代码。
func dispatch[Req any, Resp any](h *Handler, fn func(context.Context, *Req) (*Resp, error)) routeHandler {
	return func(ctx context.Context, data []byte) (any, error) {
		var req Req
		if err := json.Unmarshal(data, &req); err != nil {
			return nil, &APIError{
				Code:    "invalid_request",
				Message: "failed to parse request body",
				Details: err.Error(),
			}
		}
		return fn(ctx, &req)
	}
}

// ========== APIError 统一错误响应 ==========

// APIError 结构化 API 错误，HTTP 和 RPC 统一使用
type APIError struct {
	Code    string `json:"code"`              // 机器可读错误码，如 "session.not_found"
	Message string `json:"message"`           // 人类可读描述
	Details any    `json:"details,omitempty"` // 可选附加信息
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ========== HandleRPC ==========

// HandleRPC RPC 路由分发入口
func (h *Handler) HandleRPC(ctx context.Context, method string, data []byte) ([]byte, error) {
	start := time.Now()

	userID, _ := contextx.GetUserID(ctx)
	deviceID, _ := contextx.GetDeviceID(ctx)
	logger.Info(ctx, "[RPC] ->",
		zap.String("method", method),
		zap.String("user", userID.String()),
		zap.String("device", deviceID))

	route, ok := h.routes[protocol.RpcMethod(method)]
	if !ok {
		return nil, &APIError{
			Code:    "method_not_found",
			Message: fmt.Sprintf("unknown RPC method: %s", method),
		}
	}

	resp, err := route(ctx, data)
	if err != nil {
		logger.Warn(ctx, "[RPC] <- err",
			zap.String("method", method),
			zap.Error(err),
			zap.Duration("elapsed", time.Since(start)))
		return nil, err
	}

	result, jsonErr := json.Marshal(resp)
	if jsonErr != nil {
		logger.Error(ctx, "[RPC] marshal response failed",
			zap.String("method", method),
			zap.Error(jsonErr))
		return nil, &APIError{
			Code:    "internal_error",
			Message: "failed to serialize response",
		}
	}
	logger.Info(ctx, "[RPC] <- ok",
		zap.String("method", method),
		zap.Duration("elapsed", time.Since(start)))
	return result, nil
}
