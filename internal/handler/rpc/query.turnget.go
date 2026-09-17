package rpchandler

import (
	"context"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TurnGet retrieves a single Turn in the current user's session.
func (h *Handler) TurnGet(ctx context.Context, req *protocol.TurnGetRequest) (*protocol.TurnGetResponse, error) {
	turn, _, err := getOwnedByID(
		ctx,
		h,
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
