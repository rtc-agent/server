package agent

import (
	"context"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
)

// =============================================================================
// Internal helpers
// =============================================================================

// batchLifecyclePublish combines a turn.updated and session.updated event into a
// single UpdatePublishItem and a single Publish call, reducing centrifuge, Redis,
// and DB round-trips. Each lifecycle callback (begin, complete, interrupt, resume,
// fail, cancel) emits exactly these two events; publishing them separately would
// cost 2 Redis calls + 2 DB inserts + 2 centrifuge publishes per callback.
//
// The session is loaded once and shared across both update builders to avoid
// duplicate DB queries.
func (h *helpers) batchLifecyclePublish(ctx context.Context, turnID uuid.UUID, sessionID uuid.UUID, action string) {
	if h.deps.UpdatePublisher == nil {
		return
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		h.logIfEnabled(ctx, "batchLifecyclePublish.load_session_failed", map[string]any{
			"turn_id":    turnID.String(),
			"session_id": sessionID.String(),
			"action":     action,
			"error":      err.Error(),
		})
		return
	}

	// Build both update item sets from the same session load.
	turnUpdates := primitives.BuildTurnUpdatedUpdates(session, turnID)
	sessionUpdates := primitives.BuildSessionUpdatedUpdates(session)

	// Merge all UpdateItems into a single UpdatePublishItem so the publisher
	// persists one UserUpdate row and sends one centrifuge message containing
	// both entity updates. Without merging, each Build* result is a separate
	// UpdatePublishItem → separate DB row → separate centrifuge publish.
	var allItems []protocol.UpdateItem
	ch := ""
	for _, u := range turnUpdates {
		if ch == "" {
			ch = u.Channel
		}
		allItems = append(allItems, u.Items...)
	}
	for _, u := range sessionUpdates {
		if ch == "" {
			ch = u.Channel
		}
		allItems = append(allItems, u.Items...)
	}

	if len(allItems) == 0 {
		return
	}

	merged := []updates.UpdatePublishItem{{Channel: ch, Items: allItems}}
	if _, err := h.deps.UpdatePublisher.Publish(ctx, merged...); err != nil {
		h.logIfEnabled(ctx, "batchLifecyclePublish.publish_failed", map[string]any{
			"turn_id":    turnID.String(),
			"session_id": sessionID.String(),
			"action":     action,
			"error":      err.Error(),
		})
	}
}
