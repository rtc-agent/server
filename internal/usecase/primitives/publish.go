// internal/usecase/primitives/publish.go
package primitives

import (
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// isSystemSession reports whether the session belongs to the system.
// Updates are not published for system sessions.
//
// nil session is treated as non-system. Callers should check for nil session
// separately and skip publishing if the routing metadata is unavailable.
func isSystemSession(session *model.Session) bool {
	if session == nil {
		return false
	}
	return session.OwnerKind == string(usecase.CreatorKindSystem)
}

// BuildSendMessageUpdates builds the UpdatePublishItem list for SendMessage.
//
// turnID is optional: when non-nil, a "turn created" update is emitted; when
// nil, only the session and message updates are produced. The turn may not
// exist yet at the time the message is created — the turn is created later
// by turn-agent's CreateTurn callback when the worker processes the work
// item. In that case the caller passes nil for turnID and the frontend will
// learn about the turn when the worker actually creates it.
//
// messageIDs accepts one or more message IDs (e.g., prompt message + user message).
// Each ID produces a "message created" update item.
//
// A nil session means we cannot route the update (no OwnerRefID). Returns
// nil in that case — the caller should have already logged the pre-load
// failure.
func BuildSendMessageUpdates(
	session *model.Session,
	sessionCreated bool,
	turnID *uuid.UUID,
	messageIDs []uuid.UUID,
) []updates.UpdatePublishItem {
	if session == nil {
		return nil
	}
	if isSystemSession(session) {
		return nil
	}
	sessionAction := protocol.ActionUpdated
	if sessionCreated {
		sessionAction = protocol.ActionCreated
	}
	items := []protocol.UpdateItem{
		{Entity: protocol.EntitySession, Action: sessionAction, EntityId: session.ID.String()},
	}
	if turnID != nil {
		items = append(items, protocol.UpdateItem{
			Entity: protocol.EntityTurn, Action: protocol.ActionCreated, EntityId: turnID.String(),
		})
	}
	for _, msgID := range messageIDs {
		items = append(items, protocol.UpdateItem{
			Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: msgID.String(),
		})
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items:   items,
	}}
}

// BuildMessageUpdate builds an UpdatePublishItem for "message created" only
// (used by the worker).
// nil session returns nil (cannot route).
func BuildMessageUpdate(session *model.Session, messageID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: messageID.String(),
		}},
	}}
}

// BuildSessionUpdateUpdates builds an UpdatePublishItem for "session attributes
// updated" only.
// nil session returns nil (cannot route).
func BuildSessionUpdateUpdates(session *model.Session) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntitySession, Action: protocol.ActionUpdated, EntityId: session.ID.String(),
		}},
	}}
}

// BuildSessionCloseUpdates builds an UpdatePublishItem for "session closed".
func BuildSessionCloseUpdates(session *model.Session) []updates.UpdatePublishItem {
	return BuildSessionUpdateUpdates(session)
}

// BuildTurnStopUpdates builds an UpdatePublishItem for "turn stopped".
// nil session returns nil (cannot route).
func BuildTurnStopUpdates(session *model.Session, turnID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntityTurn, Action: protocol.ActionUpdated, EntityId: turnID.String(),
		}},
	}}
}

// BuildRtcStatusUpdates builds an UpdatePublishItem for "RTC status updated".
// nil session returns nil (cannot route).
func BuildRtcStatusUpdates(session *model.Session, rtcID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntityRtc, Action: protocol.ActionUpdated, EntityId: rtcID.String(),
		}},
	}}
}

// BuildRtcResultUpdates builds an UpdatePublishItem for "RTC result submitted".
func BuildRtcResultUpdates(session *model.Session, rtcID uuid.UUID) []updates.UpdatePublishItem {
	return BuildRtcStatusUpdates(session, rtcID)
}

// BuildOrphanTurnUpdates builds an UpdatePublishItem for "orphan RTC triggers
// a new turn". Emits both a new turn and the triggering message's creation
// notifications.
// nil session returns nil (cannot route).
func BuildOrphanTurnUpdates(session *model.Session, turnID, messageID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{
			{Entity: protocol.EntityTurn, Action: protocol.ActionCreated, EntityId: turnID.String()},
			{Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: messageID.String()},
		},
	}}
}

// BuildTurnCreatedUpdates builds an UpdatePublishItem for "turn created".
// Emitted when the createTurn callback creates a new turn, notifying the
// frontend that processing has begun. This is the only site that publishes
// turn.created — every other turn lifecycle transition publishes turn.updated.
// nil session returns nil (cannot route).
func BuildTurnCreatedUpdates(session *model.Session, turnID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntityTurn, Action: protocol.ActionCreated, EntityId: turnID.String(),
		}},
	}}
}

// BuildSessionUpdatedUpdates builds an UpdatePublishItem for "session
// attributes updated". Emitted when session state changes (e.g. beginTurn
// transitions to active, completeTurn transitions to idle).
// nil session returns nil (cannot route).
func BuildSessionUpdatedUpdates(session *model.Session) []updates.UpdatePublishItem {
	return BuildSessionUpdateUpdates(session)
}

// BuildTurnUpdatedUpdates builds an UpdatePublishItem for "turn updated".
// Emitted when turn state changes (e.g. begin to running, cancel to cancelled).
// nil session returns nil (cannot route).
func BuildTurnUpdatedUpdates(session *model.Session, turnID uuid.UUID) []updates.UpdatePublishItem {
	return BuildTurnStopUpdates(session, turnID)
}

// BuildForkSessionUpdates builds the UpdatePublishItem list for ForkSession.
// Each message gets its own UpdatePublishItem so that the downstream save()
// creates one user_updates row per message, keeping history pagination
// fine-grained for the frontend.
// nil newSession returns nil (cannot route).
func BuildForkSessionUpdates(
	newSession *model.Session,
	messages []*model.Message,
) []updates.UpdatePublishItem {
	if newSession == nil || isSystemSession(newSession) {
		return nil
	}
	ch := channel.UserTopic(newSession.OwnerRefID)
	result := make([]updates.UpdatePublishItem, 0, 1+len(messages))

	// Session creation as its own update.
	result = append(result, updates.UpdatePublishItem{
		Channel: ch,
		Items: []protocol.UpdateItem{
			{Entity: protocol.EntitySession, Action: protocol.ActionCreated, EntityId: newSession.ID.String()},
		},
	})

	// One update per message — keeps user_updates rows fine-grained.
	for _, msg := range messages {
		result = append(result, updates.UpdatePublishItem{
			Channel: ch,
			Items: []protocol.UpdateItem{
				{Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: msg.ID.String()},
			},
		})
	}
	return result
}
