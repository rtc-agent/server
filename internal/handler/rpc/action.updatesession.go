// internal/rpchandler/action.updatesession.go
package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/infra/context"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
)

// UpdateSession 更新会话（目前仅支持标题）。
func (h *Handler) UpdateSession(ctx context.Context, req *protocol.UpdateSessionRequest) (*protocol.UpdateSessionResponse, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	creator := usecase.UserCreator{UserID: userID}

	sessionUUID, apiErr := parseUUID(req.SessionId, "session_id")
	if apiErr != nil {
		return nil, apiErr
	}

	logger.Info("[UpdateSession] user=%s session=%s", userID, req.SessionId)

	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, sessionUUID, creator); err != nil {
		return nil, &APIError{Code: "permission_denied", Message: err.Error()}
	}

	fields := map[string]any{}
	if req.Title != nil {
		fields["title"] = *req.Title
	}
	if len(fields) == 0 {
		return &protocol.UpdateSessionResponse{
			Result: protocol.UpdateSessionResult{SessionId: req.SessionId},
		}, nil
	}

	// Load session BEFORE the transaction for event publishing.
	// Event publishing is best-effort — the OwnerRefID (used for routing)
	// never changes after session creation, so pre-transaction data is
	// sufficient. If the pre-load fails, we proceed with nil session and
	// the update publisher skips event delivery; the frontend will reload
	// the latest state when it notices the session title change.
	sessionBefore, sessionLoadErr := h.deps.SessionRepo.GetByID(ctx, sessionUUID)
	if sessionLoadErr != nil {
		logger.Warn("[UpdateSession] pre-load session failed (will skip event publishing): session=%s err=%v",
			sessionUUID, sessionLoadErr)
	}

	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := primitives.UpdateSessionFields(txCtx, h.deps.Deps, sessionUUID, fields); err != nil {
			return nil, err
		}
		// Use pre-loaded session for routing metadata. Do not reload inside
		// the transaction — a transient DB error on reload must not abort
		// the session field update.
		return primitives.BuildSessionUpdateUpdates(sessionBefore), nil
	})
	if err != nil {
		return nil, &APIError{Code: "update.error", Message: err.Error()}
	}

	return &protocol.UpdateSessionResponse{
		Result:  protocol.UpdateSessionResult{SessionId: req.SessionId},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}
