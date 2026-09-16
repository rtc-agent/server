package rpchandler

import (
	"context"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TurnGet 获取当前用户会话中的单个 Turn。
func (h *Handler) TurnGet(ctx context.Context, req *protocol.TurnGetRequest) (*protocol.TurnGetResponse, error) {
	turn, _, err := h.getOwnedByID(
		ctx,
		req.TurnId,
		"turn_id",
		"turn.not_found",
		"turn",
		h.deps.Deps.TurnRepo.GetByID,
		func(t *model.Turn) uuid.UUID { return t.SessionID },
	)
	if err != nil {
		return nil, err
	}
	return &protocol.TurnGetResponse{
		Item: model.ToProtocolTurn(turn),
	}, nil
}
