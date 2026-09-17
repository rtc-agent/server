// Package rpchandler provides the protocol adaptation layer for Centrifuge RPC interfaces.
//
// Each RPC method corresponds to a handler function, registered via registerRoutes.
// All externally returned errors must use the APIError type to avoid leaking internal details.
package rpchandler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	hibikenasynq "github.com/hibiken/asynq"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// Dependencies required by the RPC Handler.
type Dependencies struct {
	Deps                *usecase.Dependencies
	SessionRepo         repo.SessionRepo
	Queue               *rtcqueue.Queue // rtc-queue for publishing/cancelling work items
	API                 config.APIConfig
	ScriptExecutionRepo repo.ScriptExecutionRepo     // script execution persistence
	Metrics             *turnagent.PrometheusMetrics // Prometheus metrics
	AsynqInspector      *hibikenasynq.Inspector      // asynq inspector for loop task cleanup
}

// Handler is the RPC handler.
type Handler struct {
	deps     *Dependencies
	routes   map[protocol.RpcMethod]routeHandler
	recorder *scriptExecutionRecorder // asynchronously records script execution details
}

// routeHandler is the handler function for a single route.
type routeHandler func(ctx context.Context, data []byte) (any, error)

// NewHandler creates an RPC handler.
func NewHandler(deps *Dependencies) *Handler {
	h := &Handler{
		deps: deps,
		recorder: newScriptExecutionRecorder(deps,
			4,    // workerCount: 4 concurrent writers
			1000, // bufferSize: queue capacity 1000
		),
	}
	h.registerRoutes()
	return h
}

// Close releases resources held by the Handler (stops recorder workers).
func (h *Handler) Close() {
	if h.recorder != nil {
		h.recorder.shutdown()
	}
}

// registerRoutes registers all RPC routes.
// To add a new RPC, just add one line here; no need to modify HandleRPC.
func (h *Handler) registerRoutes() {
	h.routes = map[protocol.RpcMethod]routeHandler{
		// Session
		protocol.MethodSessionList:    dispatch(h.ListSessions),
		protocol.MethodSessionGet:     dispatch(h.GetSession),
		protocol.MethodSessionClose:   dispatch(h.CloseSession),
		protocol.MethodSessionCompact: dispatch(h.CompactSession),
		protocol.MethodSessionUpdate:  dispatch(h.UpdateSession),
		protocol.MethodSessionFork:    dispatch(h.ForkSession),

		// Message
		protocol.MethodMessageSend: dispatch(h.SendMessage),
		protocol.MethodMessageList: dispatch(h.MessageList),
		protocol.MethodMessageGet:  dispatch(h.MessageGet),

		// Turn
		protocol.MethodTurnList: dispatch(h.TurnList),
		protocol.MethodTurnGet:  dispatch(h.TurnGet),
		protocol.MethodTurnStop: dispatch(h.StopTurn),

		// RTC
		protocol.MethodRtcList:         dispatch(h.RtcList),
		protocol.MethodRtcGet:          dispatch(h.RtcGet),
		protocol.MethodRtcUpdateStatus: dispatch(h.UpdateRtcStatus),
		protocol.MethodRtcSubmitResult: dispatch(h.SubmitRtcResult),
	}
}

// dispatch is a generic dispatch helper: deserializes request -> calls handler -> returns result.
// Eliminates repetitive Unmarshal boilerplate in each case.
func dispatch[Req any, Resp any](fn func(context.Context, *Req) (*Resp, error)) routeHandler {
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

// ========== APIError unified error response ==========

// APIError is a structured API error, used uniformly by both HTTP and RPC.
type APIError struct {
	Code    string `json:"code"`              // machine-readable error code, e.g. "session.not_found"
	Message string `json:"message"`           // human-readable description
	Details any    `json:"details,omitempty"` // optional additional information
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// SafeMessage returns an error description safe for clients (without internal details).
// Used by the svc layer to extract a safe error message without importing rpchandler.
func (e *APIError) SafeMessage() string {
	return e.Error()
}

// ========== HandleRPC ==========

// HandleRPC is the RPC routing dispatch entry point.
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
