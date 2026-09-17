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

// ResolveMessageContent returns the full content of a message as a JSON string.
// If the message is still streaming, chunks are aggregated from the StreamStore
// and used to replace ContentData.Data. The return value is a ContentData JSON
// string that callers can directly unmarshal into protocol.ContentData.
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

// publishLive pushes updates to the Live channel (no offset persistence,
// real-time only). Live channels provide real-time display; after reconnect,
// clients recover state via Topic channels.
func (u *UpdatePublisher) publishLive(ctx context.Context, items []UpdatePublishItem) ([]*protocol.Update, error) {
	u.mu.RLock()
	broker := u.broker
	u.mu.RUnlock()

	var allUpdates []*protocol.Update

	for _, item := range items {
		tempUpdate := &model.UserUpdate{
			Items:  model.UpdateItemArray(item.Items),
			Offset: 0, // Live channels do not use offset
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

// publishTopic saves updates to the Topic channel (persistent offset + DB) and
// pushes them to Centrifuge.
func (u *UpdatePublisher) publishTopic(ctx context.Context, items []UpdatePublishItem) ([]*protocol.Update, error) {
	userUpdates, err := u.Save(ctx, items...)
	if err != nil {
		return nil, fmt.Errorf("save updates: %w", err)
	}
	return u.Push(ctx, items, userUpdates)
}

// entityRef records metadata for an entity reference, used in Phase 3 to
// backfill results in their original order.
type entityRef struct {
	entityType string
	entityID   protocol.UUID
	uuid       uuid.UUID
}

// collectEntityRefs iterates all UserUpdates and collects UUIDs to query,
// grouped by entity type (deduplicated). Returns allRefs preserving the
// original order and groupedIDs grouped by type.
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

// buildUpdates backfills query results into protocol.Update DataList in the
// original order.
func buildUpdates(uus []*model.UserUpdate, allRefs []entityRef, resolved map[string]map[uuid.UUID]any) []*protocol.Update {
	refIdx := 0
	result := make([]*protocol.Update, 0, len(uus))

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

// uniqueUUIDs deduplicates a UUID slice, preserving first-occurrence order.
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

// routePublishItems classifies UpdatePublishItems by channel type (Topic / Live).
// Extracted as a shared function to avoid duplicating the classification logic
// in the Publish method.
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
