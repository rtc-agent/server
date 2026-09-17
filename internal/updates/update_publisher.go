// Package updates provides the UpdatePublisher implementation, which persists
// entity change events and pushes them to Centrifuge for real-time delivery.
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"gorm.io/gorm"
)

// ErrPushAfterCommit indicates that the transaction was committed successfully,
// but the subsequent push to Centrifuge failed. Callers can detect this with
// errors.Is: the data is safely persisted, and the push failure does not
// affect the business result.
var ErrPushAfterCommit = errors.New("push failed after successful commit")

// Broker is the publish interface used to break the cyclic dependency between
// UpdatePublisher and the concrete broker implementation.
type Broker interface {
	PublishWithContext(ctx context.Context, channel string, data []byte, opts centrifuge.PublishOptions) (centrifuge.PublishResult, error)
	// PublishWithUserOffset publishes a message using a caller-allocated
	// user_update offset, guaranteeing that the outer Publication.Offset
	// matches the inner data.offset.
	PublishWithUserOffset(ctx context.Context, channel string, data []byte, offset uint32, opts centrifuge.PublishOptions) (centrifuge.PublishResult, error)
}

// EntityResolver batch-resolves rich content by entity type.
// Given a set of UUIDs, returns an id->entity map. IDs not found are
// omitted from the map (callers treat them as "deleted").
type EntityResolver func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error)

// StreamStoreAccessor provides the ability to read streaming message chunks.
// Implemented by agent.NewStreamStore and injected via SetStreamStore.
// The interface avoids importing the agent package from updates (potential
// cyclic dependency) and simplifies test mocking.
type StreamStoreAccessor interface {
	GetAllChunks(messageID string) ([]string, error)
}

// UpdatePublisher is the user update publisher.
// It persists entity change events to the database (topic channel) and
// pushes them to clients via Centrifuge.
type UpdatePublisher struct {
	db                   *gorm.DB
	redis                redis.UniversalClient
	mu                   sync.RWMutex // guards concurrent read/write of broker and streamStore
	broker               Broker
	resolvers            map[string]EntityResolver
	streamStore          StreamStoreAccessor // optional: reads chunks for streaming-status messages
	compressionThreshold atomic.Int64        // compression trigger threshold, used for token estimate fields
}

// buildRepoResolver creates a resolver function for entities that follow the
// standard pattern: GetByIDs + nil placeholder + protocol conversion.
// Shared by Turn and Rtc resolvers to avoid duplication.
func buildRepoResolver[T any, P any](
	getByIDs func(context.Context, []uuid.UUID) (map[uuid.UUID]*T, error),
	toProtocol func(*T) P,
) func(context.Context, []uuid.UUID) (map[uuid.UUID]any, error) {
	return func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
		return resolveWithNilPlaceholder(ctx, ids, func(ctx context.Context, realIDs []uuid.UUID) (map[uuid.UUID]any, error) {
			items, err := getByIDs(ctx, realIDs)
			if err != nil {
				return nil, err
			}
			result := make(map[uuid.UUID]any, len(items))
			for id, item := range items {
				result[id] = toProtocol(item)
			}
			return result, nil
		})
	}
}

// NewUpdatePublisher creates a new UpdatePublisher.
func NewUpdatePublisher(
	db *gorm.DB,
	redis redis.UniversalClient,
	sessionRepo repo.SessionRepo,
	messageRepo repo.MessageRepo,
	turnRepo repo.TurnRepo,
	rtcRepo repo.RtcRepo,
) *UpdatePublisher {
	u := &UpdatePublisher{
		db:        db,
		redis:     redis,
		resolvers: make(map[string]EntityResolver),
	}

	// Register entity resolvers (registry pattern, batch query).
	u.resolvers[string(protocol.EntitySession)] = func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
		sessions, err := sessionRepo.GetByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		result := make(map[uuid.UUID]any, len(sessions))
		for id, s := range sessions {
			ps := toProtocolSession(s)
			// Compute token estimate fields from the session's persisted EWMA.
			enrichSessionWithTokenEstimate(&ps, s, u.compressionThreshold.Load())
			result[id] = ps
		}
		return result, nil
	}
	u.resolvers[string(protocol.EntityMessage)] = func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
		messages, err := messageRepo.GetByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}

		// Streaming messages: read chunks from Redis and append to content.
		// Race condition: chunks may not be fully written yet; we accept
		// eventual consistency (the frontend gets the latest content via
		// a subsequent update).
		u.mu.RLock()
		ss := u.streamStore
		u.mu.RUnlock()

		result := make(map[uuid.UUID]any, len(messages))
		for id, m := range messages {
			if ss != nil && protocol.MessageStreamingStatus(m.StreamingStatus) == protocol.MessageStreamingStreaming {
				chunks, chunksErr := ss.GetAllChunks(m.ID.String())
				if chunksErr != nil {
					// Fallback: return raw DB data and log a warning.
					logger.Warn(ctx, "[UpdatePublisher] get chunks for streaming message failed, falling back to DB data",
						zap.String("message_id", m.ID.String()),
						zap.Error(chunksErr),
					)
				} else if len(chunks) > 0 {
					v := &protocol.ContentData{}
					if e := json.Unmarshal([]byte(m.Content), v); e != nil {
						logger.Warn(ctx, "[UpdatePublisher] unmarshal content failed", zap.Error(e))
						m.Content = strings.Join(chunks, "")
					} else {
						v.Data = strings.Join(chunks, "")
						if tmp, marshalErr := json.Marshal(v); marshalErr != nil {
							logger.Warn(ctx, "[UpdatePublisher] marshal content with chunks failed", zap.Error(marshalErr))
							m.Content = strings.Join(chunks, "")
						} else {
							m.Content = string(tmp)
						}
					}
				}
			}
			result[id] = toProtocolMessage(m)
		}
		return result, nil
	}
	u.resolvers[string(protocol.EntityTurn)] = buildRepoResolver(turnRepo.GetByIDs, toProtocolTurn)
	u.resolvers[string(protocol.EntityRtc)] = buildRepoResolver(rtcRepo.GetByIDs, toProtocolRtc)

	return u
}

// SetBroker injects the broker (breaks cyclic dependency: broker creation
// needs UpdatePublisher as HistoryStore).
func (u *UpdatePublisher) SetBroker(broker Broker) {
	u.mu.Lock()
	u.broker = broker
	u.mu.Unlock()
}

// SetStreamStore injects the StreamStoreAccessor (for reading chunks of
// streaming-status messages). Optional: if not called, streaming messages
// return raw DB data (content may be incomplete).
func (u *UpdatePublisher) SetStreamStore(s StreamStoreAccessor) {
	u.mu.Lock()
	u.streamStore = s
	u.mu.Unlock()
}

// SetCompressionThreshold sets the compression trigger threshold
// (contextTokensLimit - autoCompactBufferTokens). Used for computing
// token estimate fields (compression_progress, rounds_until_compression,
// estimated_next_round_tokens).
func (u *UpdatePublisher) SetCompressionThreshold(threshold int64) {
	u.compressionThreshold.Store(threshold)
}

// UpdatePublishItem represents a single update event describing a batch of entity changes.
type UpdatePublishItem struct {
	Channel string
	Items   []protocol.UpdateItem
}

// ========== Two-phase publish (recommended) ==========

// Save persists UserUpdate records within a transaction (generates offset +
// writes to DB). The caller manages the transaction:
// Begin -> WithTx(ctx) -> Save -> Commit.
func (u *UpdatePublisher) Save(ctx context.Context, items ...UpdatePublishItem) ([]*model.UserUpdate, error) {
	return u.save(ctx, items...)
}

// Push converts saved UserUpdate records into rich content and pushes them
// to Centrifuge. Should be called after the transaction commits, so that
// subscribers can see the committed data when querying.
func (u *UpdatePublisher) Push(ctx context.Context, items []UpdatePublishItem, savedUpdates []*model.UserUpdate) ([]*protocol.Update, error) {
	pushUpdates, err := u.convertUpdates(ctx, savedUpdates)
	if err != nil {
		return nil, fmt.Errorf("convert updates: %w", err)
	}
	return u.publishUpdates(ctx, items, pushUpdates)
}

// RunAndPublish executes fn within a transaction, collects the
// UpdatePublishItems to publish, then pushes them to Centrifuge after
// commit. All DB writes within fn should use the provided txCtx.
//
// Flow: Begin tx -> fn(txCtx) returns items -> save(items) -> commit -> push.
// Any failure rolls back the transaction and returns an error. If commit
// succeeds but push fails, the error is wrapped with ErrPushAfterCommit;
// callers can detect it with errors.Is and safely ignore it (data is
// persisted, and the client will sync via reconnect).
func (u *UpdatePublisher) RunAndPublish(
	ctx context.Context,
	fn func(txCtx context.Context) ([]UpdatePublishItem, error),
) ([]*protocol.Update, error) {
	tx := u.db.Begin()
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback()
			panic(r)
		}
	}()
	txCtx := repo.WithTx(ctx, tx)

	items, err := fn(txCtx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	saved, err := u.save(txCtx, items...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	// Push after successful commit; wrap push failure with ErrPushAfterCommit.
	pushUpdates, pushErr := u.Push(ctx, items, saved)
	if pushErr != nil {
		logger.Error(ctx, "[UpdatePublisher] push failed (data already committed)", zap.Error(pushErr))
		return pushUpdates, fmt.Errorf("%w: %v", ErrPushAfterCommit, pushErr)
	}
	return pushUpdates, nil
}

// publishUpdates publishes the converted []*protocol.Update to the
// corresponding channels in items order.
func (u *UpdatePublisher) publishUpdates(ctx context.Context, items []UpdatePublishItem, pushUpdates []*protocol.Update) ([]*protocol.Update, error) {
	u.mu.RLock()
	broker := u.broker
	u.mu.RUnlock()

	pushIdx := 0
	for _, item := range items {
		for range item.Items {
			if pushIdx >= len(pushUpdates) {
				break
			}
			update := pushUpdates[pushIdx]
			pushIdx++

			data, err := json.Marshal(update)
			if err != nil {
				return nil, fmt.Errorf("marshal update: %w", err)
			}

			_, err = broker.PublishWithUserOffset(ctx, item.Channel, data, update.Offset, centrifuge.PublishOptions{})
			if err != nil {
				logger.Error(ctx, "[UpdatePublisher] publish to centrifuge failed",
					zap.String("channel", item.Channel),
					zap.String("update_id", update.Id),
					zap.Error(err))
				return nil, fmt.Errorf("publish to centrifuge: %w", err)
			}
			logger.Debug(ctx, "[UpdatePublisher] published update",
				zap.String("channel", item.Channel),
				zap.Uint32("offset", update.Offset),
				zap.Int("items", len(update.Items)))
		}
	}
	return pushUpdates, nil
}

// ========== Single-shot publish (legacy compatibility) ==========

// Publish publishes update events (called outside a transaction).
func (u *UpdatePublisher) Publish(ctx context.Context, items ...UpdatePublishItem) ([]*protocol.Update, error) {
	topicItems, liveItems := routePublishItems(items)

	var allUpdates []*protocol.Update

	if len(topicItems) > 0 {
		updates, err := u.publishTopic(ctx, topicItems)
		if err != nil {
			return nil, fmt.Errorf("publish topic: %w", err)
		}
		allUpdates = append(allUpdates, updates...)
	}

	if len(liveItems) > 0 {
		updates, err := u.publishLive(ctx, liveItems)
		if err != nil {
			return nil, fmt.Errorf("publish live: %w", err)
		}
		allUpdates = append(allUpdates, updates...)
	}

	return allUpdates, nil
}

func (u *UpdatePublisher) save(ctx context.Context, items ...UpdatePublishItem) ([]*model.UserUpdate, error) {
	channelItemsMap := make(map[string][]UpdatePublishItem)
	for _, item := range items {
		channelItemsMap[item.Channel] = append(channelItemsMap[item.Channel], item)
	}

	channels := make([]string, 0, len(channelItemsMap))
	batchSizes := make([]int, 0, len(channelItemsMap))
	// 排序 channel 确保遍历顺序确定（map 迭代顺序不确定），便于调试与日志追踪。
	for ch := range channelItemsMap {
		channels = append(channels, ch)
	}
	sort.Strings(channels)
	for _, ch := range channels {
		batchSizes = append(batchSizes, len(channelItemsMap[ch]))
	}

	offsetKeys := make([]string, len(channels))
	for i, ch := range channels {
		offsetKeys[i] = cache.ChannelOffset(ch)
	}

	argv := make([]any, len(batchSizes))
	for i, size := range batchSizes {
		argv[i] = size
	}

	maxOffsetsResult, err := cache.BatchIncrOffset.Run(ctx, u.redis, offsetKeys, argv...).Result()
	if err != nil {
		return nil, fmt.Errorf("batch incr offset: %w", err)
	}

	maxOffsets, ok := maxOffsetsResult.([]any)
	if !ok || len(maxOffsets) != len(channels) {
		return nil, fmt.Errorf("invalid batch incr offset result")
	}

	var userUpdates []*model.UserUpdate
	for i, ch := range channels {
		userIDStr, ok := channel.ParseUser(ch)
		if !ok {
			return nil, fmt.Errorf("invalid channel format: %s", ch)
		}

		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return nil, fmt.Errorf("parse user ID: %w", err)
		}

		maxOffsetRaw, ok := maxOffsets[i].(int64)
		if !ok {
			return nil, fmt.Errorf("invalid offset type for channel %s", ch)
		}
		maxOffset := uint32(maxOffsetRaw)
		batchSize := uint32(batchSizes[i])
		startOffset := maxOffset - batchSize + 1

		for j, item := range channelItemsMap[ch] {
			offset := startOffset + uint32(j)
			userUpdate := &model.UserUpdate{
				UserID: userID,
				Offset: offset,
				Items:  model.UpdateItemArray(item.Items),
			}
			userUpdates = append(userUpdates, userUpdate)
		}
	}

	if len(userUpdates) == 0 {
		return nil, nil // nothing to persist — avoids gorm error on empty slice Create
	}

	if err := repo.DBFromContext(ctx, u.db).WithContext(ctx).Create(&userUpdates).Error; err != nil {
		logger.Error(ctx, "[UpdatePublisher] batch insert user_updates failed",
			zap.Int("count", len(userUpdates)),
			zap.Error(err))
		return nil, fmt.Errorf("batch insert user updates: %w", err)
	}

	logger.Debug(ctx, "[UpdatePublisher] saved user_updates", zap.Int("count", len(userUpdates)))
	return userUpdates, nil
}

// convertUpdates 将瘦引用（model.UserUpdate）转换成富内容（protocol.Update）。
// 按实体类型分组批量查询，避免 N+1 问题：N 个 item 只需 E 次 DB 查询（E = 实体类型数）。
//
// 三阶段流程：
//  1. collectEntityRefs — 按实体类型收集所有需要查询的 ID（去重）
//  2. resolveEntities  — 按实体类型批量查询
//  3. buildUpdates     — 按原始顺序回填 dataList
func (u *UpdatePublisher) convertUpdates(ctx context.Context, uus []*model.UserUpdate) ([]*protocol.Update, error) {
	allRefs, groupedIDs, err := collectEntityRefs(uus, u.resolvers)
	if err != nil {
		return nil, err
	}

	resolved, err := u.resolveEntities(ctx, groupedIDs)
	if err != nil {
		return nil, err
	}

	return buildUpdates(uus, allRefs, resolved), nil
}

// resolveEntities 按实体类型批量查询富内容。
func (u *UpdatePublisher) resolveEntities(ctx context.Context, groupedIDs map[string][]uuid.UUID) (map[string]map[uuid.UUID]any, error) {
	resolved := make(map[string]map[uuid.UUID]any, len(groupedIDs))
	for entity, ids := range groupedIDs {
		if len(ids) == 0 {
			continue
		}
		resolver := u.resolvers[entity]
		data, err := resolver(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("batch resolve entity %s: %w", entity, err)
		}
		resolved[entity] = data
	}
	return resolved, nil
}
