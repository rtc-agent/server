package rpchandler

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TurnGet 获取当前用户会话中的单个 Turn。
func (h *Handler) TurnGet(ctx context.Context, req *protocol.TurnGetRequest) (*protocol.TurnGetResponse, error) {
	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, err
	}

	turnUUID, apiErr := parseUUID(req.TurnId, "turn_id")
	if apiErr != nil {
		return nil, apiErr
	}

	turn, err := h.deps.Deps.TurnRepo.GetByID(ctx, turnUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{
				Code:    "turn.not_found",
				Message: fmt.Sprintf("turn %s not found", req.TurnId),
			}
		}
		return nil, &APIError{Code: "turn.error", Message: err.Error()}
	}

	if _, err := h.loadOwnedSession(ctx, turn.SessionID, userID); err != nil {
		return nil, err
	}

	logger.Info("[TurnGet] user=%s turn=%s", userID, req.TurnId)
	return &protocol.TurnGetResponse{
		Item: model.ToProtocolTurn(turn),
	}, nil
}
