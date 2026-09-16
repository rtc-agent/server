package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ResolveMessageContent 返回消息的完整内容（JSON 字符串）。
// 如果消息是 streaming 状态，从 StreamStore 聚合 chunks 并替换 ContentData.Data。
// 返回值为 ContentData JSON 字符串，调用方可直接 Unmarshal 为 protocol.ContentData。
func (u *UpdatePublisher) ResolveMessageContent(msg *model.Message) string {
	u.mu.RLock()
	ss := u.streamStore
	u.mu.RUnlock()

	if ss != nil && protocol.MessageStreamingStatus(msg.StreamingStatus) == protocol.MessageStreamingStreaming {
		chunks, err := ss.GetAllChunks(msg.ID.String())
		if err == nil && len(chunks) > 0 {
			joinedChunks := strings.Join(chunks, "")
			// Try to preserve ContentData structure (type + data).
			var contentData protocol.ContentData
			if jsonErr := json.Unmarshal([]byte(msg.Content), &contentData); jsonErr == nil {
				contentData.Data = joinedChunks
				if result, marshalErr := json.Marshal(contentData); marshalErr == nil {
					return string(result)
				}
			}
			// Fallback: return chunks as plain text.
			return joinedChunks
		}
	}
	return msg.Content
}

// publishLive 将更新推送到 Live 频道（不持久化 offset，仅实时推送）。
// Live 频道用于实时展示，断线后客户端通过 Topic 频道恢复状态。
func (u *UpdatePublisher) publishLive(ctx context.Context, items []UpdatePublishItem) ([]*protocol.Update, error) {
	u.mu.RLock()
	broker := u.broker
	u.mu.RUnlock()

	var allUpdates []*protocol.Update

	for _, item := range items {
		tempUpdate := &model.UserUpdate{
			Items:  model.UpdateItemArray(item.Items),
			Offset: 0, // Live 频道不使用 offset
		}

		pushUpdates, err := u.convertUpdates(ctx, []*model.UserUpdate{tempUpdate})
		if err != nil {
			return nil, fmt.Errorf("convert updates: %w", err)
		}

		for _, update := range pushUpdates {
			data, err := json.Marshal(update)
			if err != nil {
				return nil, fmt.Errorf("marshal update: %w", err)
			}

			_, err = broker.PublishWithContext(ctx, item.Channel, data, centrifuge.PublishOptions{})
			if err != nil {
				return nil, fmt.Errorf("publish to centrifuge: %w", err)
			}
		}
		allUpdates = append(allUpdates, pushUpdates...)
	}

	return allUpdates, nil
}

// publishTopic 将更新保存到 Topic 频道（持久化 offset + DB）并推送到 Centrifuge。
func (u *UpdatePublisher) publishTopic(ctx context.Context, items []UpdatePublishItem) ([]*protocol.Update, error) {
	userUpdates, err := u.Save(ctx, items...)
	if err != nil {
		return nil, fmt.Errorf("save updates: %w", err)
	}
	return u.Push(ctx, items, userUpdates)
}

// entityRef 记录一个实体引用的元数据，用于 Phase 3 按原始顺序回填。
type entityRef struct {
	entityType string
	entityID   protocol.UUID
	uuid       uuid.UUID
}

// collectEntityRefs 遍历所有 UserUpdate，按实体类型收集需要查询的 UUID（去重）。
// 返回保持原始顺序的 allRefs 和按类型分组的 groupedIDs。
func collectEntityRefs(uus []*model.UserUpdate, resolvers map[string]EntityResolver) ([]entityRef, map[string][]uuid.UUID, error) {
	var allRefs []entityRef
	groupedIDs := make(map[string][]uuid.UUID)

	for _, uu := range uus {
		for _, item := range uu.Items {
			entityUUID, parseErr := uuid.Parse(item.EntityId)
			if parseErr != nil {
				return nil, nil, fmt.Errorf("parse entity ID %s: %w", item.EntityId, parseErr)
			}
			allRefs = append(allRefs, entityRef{
				entityType: string(item.Entity),
				entityID:   item.EntityId,
				uuid:       entityUUID,
			})
			if _, ok := resolvers[string(item.Entity)]; ok {
				groupedIDs[string(item.Entity)] = append(groupedIDs[string(item.Entity)], entityUUID)
			}
		}
	}

	for entity, ids := range groupedIDs {
		groupedIDs[entity] = uniqueUUIDs(ids)
	}

	return allRefs, groupedIDs, nil
}

// buildUpdates 按原始顺序将查询结果回填到 protocol.Update 的 DataList。
func buildUpdates(uus []*model.UserUpdate, allRefs []entityRef, resolved map[string]map[uuid.UUID]any) []*protocol.Update {
	refIdx := 0
	var result []*protocol.Update

	for _, uu := range uus {
		update := &protocol.Update{
			Id:     uu.ID.String(),
			Items:  make([]protocol.UpdateItem, len(uu.Items)),
			Offset: uu.Offset,
		}
		copy(update.Items, uu.Items)

		dataList := make([]any, len(uu.Items))
		for i := range uu.Items {
			ref := allRefs[refIdx]
			refIdx++

			entityData, hasResolver := resolved[ref.entityType]
			if !hasResolver {
				continue
			}

			if data, ok := entityData[ref.uuid]; ok {
				dataList[i] = data
			} else {
				dataList[i] = map[string]any{
					"id":         ref.entityID,
					"deleted_at": time.Now(),
				}
			}
		}

		update.DataList = &dataList
		result = append(result, update)
	}

	return result
}

// resolveWithNilPlaceholder separates nil UUIDs (treated as deleted placeholders)
// from real IDs, resolves real IDs via inner, and fills nil UUID slots with a
// tombstone entry containing id and deleted_at. This pattern is shared by entity
// resolvers that need to handle nil UUID placeholders for deleted entities.
func resolveWithNilPlaceholder(
	ctx context.Context,
	ids []uuid.UUID,
	inner func(ctx context.Context, realIDs []uuid.UUID) (map[uuid.UUID]any, error),
) (map[uuid.UUID]any, error) {
	var realIDs []uuid.UUID
	for _, id := range ids {
		if id != uuid.Nil {
			realIDs = append(realIDs, id)
		}
	}
	resolved, err := inner(ctx, realIDs)
	if err != nil {
		return nil, err
	}
	result := make(map[uuid.UUID]any, len(ids))
	for k, v := range resolved {
		result[k] = v
	}
	for _, id := range ids {
		if id == uuid.Nil {
			result[id] = map[string]any{
				"id":         id.String(),
				"deleted_at": time.Now(),
			}
		}
	}
	return result, nil
}

// uniqueUUIDs 对 UUID 切片去重，保持首次出现的顺序。
func uniqueUUIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	result := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			result = append(result, id)
		}
	}
	return result
}

// channelRouter 按频道类型（Topic / Live）分类 UpdatePublishItem。
// 提取为公共函数避免在 Publish 方法中重复分类逻辑。
func routePublishItems(items []UpdatePublishItem) (topic, live []UpdatePublishItem) {
	for _, item := range items {
		if channel.IsTopic(item.Channel) {
			topic = append(topic, item)
		} else if channel.IsLive(item.Channel) {
			live = append(live, item)
		}
	}
	return
}
