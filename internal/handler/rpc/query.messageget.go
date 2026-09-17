package rpchandler

import (
	"context"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// MessageGet 获取当前用户会话中的单个消息。
func (h *Handler) MessageGet(ctx context.Context, req *protocol.MessageGetRequest) (*protocol.MessageGetResponse, error) {
	msg, _, err := getOwnedByID(
		ctx,
		h,
		req.MessageId,
		"message_id",
		"message.not_found",
		"message",
		h.deps.Deps.MessageRepo.GetByID,
		func(m *model.Message) uuid.UUID { return m.SessionID },
	)
	if err != nil {
		return nil, err
	}
	return &protocol.MessageGetResponse{
		Item: model.ToProtocolMessage(msg),
	}, nil
}
