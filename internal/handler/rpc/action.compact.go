// internal/handler/rpc/action.compact.go
package rpchandler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"go.uber.org/zap"
)

// CompactSession triggers an explicit context compression for a session.
// The compact work item is enqueued with the same priority as submit (0).
//
// Dedup: if a compact work item is already pending or processing for this
// session, the request is rejected with "compact.already_pending".
func (h *Handler) CompactSession(ctx context.Context, req *protocol.CompactSessionRequest) (*protocol.CompactSessionResponse, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	creator := usecase.UserCreator{UserID: userID}

	sessionUUID, apiErr := parseUUID(req.SessionId, "session_id")
	if apiErr != nil {
		return nil, apiErr
	}

	logger.Info(ctx, "[CompactSession]",
		zap.String("user", userID.String()),
		zap.String("session", string(req.SessionId)))

	// 1. Session 存在性校验
	session, err := h.deps.SessionRepo.GetByID(ctx, sessionUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{
				Code:    "session.not_found",
				Message: fmt.Sprintf("session %s not found", req.SessionId),
			}
		}
		return nil, h.internalError(ctx, "session.error", "internal error", err)
	}

	// 2. 权限校验
	if session.OwnerKind != string(creator.Kind()) || session.OwnerRefID != creator.ReferenceID() {
		return nil, &APIError{
			Code:    "permission_denied",
			Message: fmt.Sprintf("session %s does not belong to user", session.ID),
		}
	}

	// 3. 防重入：检查队列中是否已有该 session 的 compact 任务
	if h.deps.Queue != nil {
		hasPending, checkErr := h.deps.Queue.HasPendingWorkByKind(ctx, session.ID.String(), string(turnagent.WorkKindCompact))
		if checkErr != nil {
			return nil, h.internalError(ctx, "compact.dedup_error", "internal error", checkErr)
		}
		if hasPending {
			return nil, &APIError{
				Code:    "compact.already_pending",
				Message: "a compact task is already pending or processing for this session",
			}
		}
	}

	// 4. 构造 payload 并入队
	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:              turnagent.WorkKindCompact,
		SessionID:         session.ID.String(),
		CustomInstruction: req.CustomInstruction,
	})
	if marshalErr != nil {
		return nil, h.internalError(ctx, "compact.marshal_error", "internal error", marshalErr)
	}

	if h.deps.Queue != nil {
		if _, err := h.deps.Queue.Publish(ctx, session.ID.String(), string(payload), 0); err != nil {
			return nil, h.internalError(ctx, "compact.queue_error", "failed to enqueue compact task", err)
		}
	}

	logger.Info(ctx, "[CompactSession] enqueued",
		zap.String("session", session.ID.String()),
		zap.Bool("has_custom_instruction", req.CustomInstruction != nil))

	return &protocol.CompactSessionResponse{
		Result: protocol.CompactSessionResult{Success: true},
	}, nil
}
