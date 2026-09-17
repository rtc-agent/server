package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.uber.org/zap"
)

// listSessionsDefaultLimit is the default page size (reuses queryMaxLimit as maximum).
const listSessionsDefaultLimit = 20

// ListSessions retrieves the current user's session list (ordered by creation time descending, cursor pagination).
// RPC-specific query, not shared with LLM; business logic is implemented directly here.
func (h *Handler) ListSessions(ctx context.Context, req *protocol.ListSessionsRequest) (*protocol.ListSessionsResponse, error) {
	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, err
	}

	limit, apiErr := clampLimit(req.Limit, listSessionsDefaultLimit, h.deps.API.QueryMaxLimit)
	if apiErr != nil {
		return nil, apiErr
	}

	sessions, err := h.deps.SessionRepo.GetByUser(ctx, userID, req.Cursor, limit)
	if err != nil {
		return nil, h.internalError(ctx, "session.list_failed", "internal error", err)
	}

	items := make([]protocol.Session, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, model.ToProtocolSession(s))
	}

	// When returned count equals limit, assume there may be a next page; use the last ID as cursor.
	var nextCursor *string
	if len(sessions) == limit {
		last := sessions[len(sessions)-1].ID.String()
		nextCursor = &last
	}

	logger.Info(ctx, "[ListSessions]",
		zap.String("user", userID.String()),
		zap.Int("count", len(items)),
		zap.Bool("has_next", nextCursor != nil))
	return &protocol.ListSessionsResponse{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}
