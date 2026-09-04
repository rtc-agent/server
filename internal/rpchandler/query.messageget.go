package rpchandler

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/dbmodel"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
)

// MessageGet 获取当前用户会话中的单个消息。
func (h *Handler) MessageGet(ctx context.Context, req *protocol.MessageGetRequest) (*protocol.MessageGetResponse, error) {
	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, err
	}

	messageUUID, apiErr := parseUUID(req.MessageId, "message_id")
	if apiErr != nil {
		return nil, apiErr
	}

	msg, err := h.deps.Deps.MessageRepo.GetByID(ctx, messageUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{
				Code:    "message.not_found",
				Message: fmt.Sprintf("message %s not found", req.MessageId),
			}
		}
		return nil, &APIError{Code: "message.error", Message: err.Error()}
	}

	if _, err := h.loadOwnedSession(ctx, msg.SessionID, userID); err != nil {
		return nil, err
	}

	logger.Info("[MessageGet] user=%s message=%s", userID, req.MessageId)
	return &protocol.MessageGetResponse{
		Item: dbmodel.ToProtocolMessage(msg),
	}, nil
}
