// internal/usecase/primitives/publish.go
package primitives

import (
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/dbmodel"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// isSystemSession 判断 session 是否属于系统。系统 session 不推送 update。
//
// nil session is treated as non-system. Callers should check for nil session
// separately and skip publishing if the routing metadata is unavailable.
func isSystemSession(session *dbmodel.Session) bool {
	if session == nil {
		return false
	}
	return session.OwnerKind == string(usecase.CreatorKindSystem)
}

// BuildSendMessageUpdates 构造 SendMessage 的 UpdatePublishItem 列表。
//
// turnID is optional: when non-nil, a "turn created" update is emitted; when
// nil, only the session and message updates are produced. The turn may not
// exist yet at the time the message is created — the turn is created later
// by turn-agent's CreateTurn callback when the worker processes the work
// item. In that case the caller passes nil for turnID and the frontend will
// learn about the turn when the worker actually creates it.
//
// A nil session means we cannot route the update (no OwnerRefID). Returns
// nil in that case — the caller should have already logged the pre-load
// failure.
func BuildSendMessageUpdates(
	session *dbmodel.Session,
	sessionCreated bool,
	turnID *uuid.UUID,
	messageID uuid.UUID,
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
		{Entity: protocol.EntitySession, Action: sessionAction, EntityId: protocol.UUID(session.ID.String())},
	}
	if turnID != nil {
		items = append(items, protocol.UpdateItem{
			Entity: protocol.EntityTurn, Action: protocol.ActionCreated, EntityId: protocol.UUID(turnID.String()),
		})
	}
	items = append(items, protocol.UpdateItem{
		Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: protocol.UUID(messageID.String()),
	})
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items:   items,
	}}
}

// BuildMessageUpdate 构造"仅 message 创建"的 UpdatePublishItem（worker 使用）。
// nil session returns nil (cannot route).
func BuildMessageUpdate(session *dbmodel.Session, messageID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: protocol.UUID(messageID.String()),
		}},
	}}
}

// BuildSessionUpdateUpdates 构造"仅 session 属性更新"的 UpdatePublishItem。
// nil session returns nil (cannot route).
func BuildSessionUpdateUpdates(session *dbmodel.Session) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntitySession, Action: protocol.ActionUpdated, EntityId: protocol.UUID(session.ID.String()),
		}},
	}}
}

// BuildSessionCloseUpdates 构造"session 关闭"的 UpdatePublishItem。
func BuildSessionCloseUpdates(session *dbmodel.Session) []updates.UpdatePublishItem {
	return BuildSessionUpdateUpdates(session)
}

// BuildTurnStopUpdates 构造"turn 停止"的 UpdatePublishItem。
// nil session returns nil (cannot route).
func BuildTurnStopUpdates(session *dbmodel.Session, turnID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntityTurn, Action: protocol.ActionUpdated, EntityId: protocol.UUID(turnID.String()),
		}},
	}}
}

// BuildRtcStatusUpdates 构造"RTC 状态更新"的 UpdatePublishItem。
// nil session returns nil (cannot route).
func BuildRtcStatusUpdates(session *dbmodel.Session, rtcID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntityRtc, Action: protocol.ActionUpdated, EntityId: protocol.UUID(rtcID.String()),
		}},
	}}
}

// BuildRtcResultUpdates 构造"RTC 结果提交"的 UpdatePublishItem。
func BuildRtcResultUpdates(session *dbmodel.Session, rtcID uuid.UUID) []updates.UpdatePublishItem {
	return BuildRtcStatusUpdates(session, rtcID)
}

// BuildOrphanTurnUpdates 构造"孤儿 RTC 触发新 turn"的 UpdatePublishItem。
// 新 Turn + 触发消息的创建通知。
// nil session returns nil (cannot route).
func BuildOrphanTurnUpdates(session *dbmodel.Session, turnID, messageID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{
			{Entity: protocol.EntityTurn, Action: protocol.ActionCreated, EntityId: protocol.UUID(turnID.String())},
			{Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: protocol.UUID(messageID.String())},
		},
	}}
}

// BuildTurnCreatedUpdates 构造"turn 创建"的 UpdatePublishItem。
// 当 createTurn 回调创建新 turn 时发布，通知前端有新的 turn 开始处理。
// 这是唯一发布 turn.created 的位置 —— 其他 turn 生命周期转换都发布 turn.updated。
// nil session returns nil (cannot route).
func BuildTurnCreatedUpdates(session *dbmodel.Session, turnID uuid.UUID) []updates.UpdatePublishItem {
	if session == nil || isSystemSession(session) {
		return nil
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(session.OwnerRefID),
		Items: []protocol.UpdateItem{{
			Entity: protocol.EntityTurn, Action: protocol.ActionCreated, EntityId: protocol.UUID(turnID.String()),
		}},
	}}
}

// BuildSessionUpdatedUpdates 构造"session 属性更新"的 UpdatePublishItem。
// 当 session 状态变化时发布（如 beginTurn → active, completeTurn → idle）。
// nil session returns nil (cannot route).
func BuildSessionUpdatedUpdates(session *dbmodel.Session) []updates.UpdatePublishItem {
	return BuildSessionUpdateUpdates(session)
}

// BuildTurnUpdatedUpdates 构造"turn 更新"的 UpdatePublishItem。
// 当 turn 状态变化时发布（如 begin → running, cancel → cancelled）。
// nil session returns nil (cannot route).
func BuildTurnUpdatedUpdates(session *dbmodel.Session, turnID uuid.UUID) []updates.UpdatePublishItem {
	return BuildTurnStopUpdates(session, turnID)
}

// BuildForkSessionUpdates 构造 ForkSession 的 UpdatePublishItem 列表。
// 新 session 创建 + 批量消息创建。
// nil newSession returns nil (cannot route).
func BuildForkSessionUpdates(
	newSession *dbmodel.Session,
	messages []*dbmodel.Message,
) []updates.UpdatePublishItem {
	if newSession == nil || isSystemSession(newSession) {
		return nil
	}
	items := []protocol.UpdateItem{
		{Entity: protocol.EntitySession, Action: protocol.ActionCreated, EntityId: protocol.UUID(newSession.ID.String())},
	}
	// 添加所有消息的 created update
	for _, msg := range messages {
		items = append(items, protocol.UpdateItem{
			Entity:   protocol.EntityMessage,
			Action:   protocol.ActionCreated,
			EntityId: protocol.UUID(msg.ID.String()),
		})
	}
	return []updates.UpdatePublishItem{{
		Channel: channel.UserTopic(newSession.OwnerRefID),
		Items:   items,
	}}
}
