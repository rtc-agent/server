// Package updates 提供 UpdatePublisher 实现，用于将实体变化事件保存并推送到 Centrifuge。
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

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

// ErrPushAfterCommit 表示事务已提交成功，但后续推送到 Centrifuge 失败。
// 调用方可通过 errors.Is 检测此错误：数据已安全持久化，推送失败不影响业务结果。
var ErrPushAfterCommit = errors.New("push failed after successful commit")

// Broker 发布接口，用于打断 UpdatePublisher 与具体 broker 实现的循环依赖。
type Broker interface {
	PublishWithContext(ctx context.Context, channel string, data []byte, opts centrifuge.PublishOptions) (centrifuge.PublishResult, error)
}

// EntityResolver 根据实体类型批量查询富内容。
// 接收一组 UUID，返回 id→实体 的 map。未找到的 ID 不出现在 map 中（调用方按"已删除"处理）。
type EntityResolver func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error)

// StreamStoreAccessor 提供读取流式消息 chunks 的能力。
// 由 agent.NewStreamStore 实现，通过 SetStreamStore 注入。
// 使用接口避免 updates 包导入 agent 包（潜在循环依赖），同时便于测试 mock。
type StreamStoreAccessor interface {
	GetAllChunks(messageID string) ([]string, error)
}

// UpdatePublisher 用户更新发布器
// 负责将实体变化事件保存到数据库（Topic 频道）并通过 Centrifuge 推送给客户端
type UpdatePublisher struct {
	db          *gorm.DB
	redis       redis.UniversalClient
	mu          sync.RWMutex // 保护 broker 和 streamStore 的并发读写
	broker      Broker
	resolvers   map[string]EntityResolver
	streamStore StreamStoreAccessor // 可选：用于读取 streaming 状态消息的 chunks
}

// NewUpdatePublisher 创建 UpdatePublisher
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

	// 注册实体解析器（registry 模式，批量查询）
	u.resolvers[string(protocol.EntitySession)] = func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
		sessions, err := sessionRepo.GetByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		result := make(map[uuid.UUID]any, len(sessions))
		for id, s := range sessions {
			result[id] = toProtocolSession(s)
		}
		return result, nil
	}
	u.resolvers[string(protocol.EntityMessage)] = func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
		messages, err := messageRepo.GetByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}

		// 流式消息：从 Redis 读取 chunks 拼接到 content。
		// 竞争条件：chunks 可能未完全写入，接受最终一致性（前端通过后续 update 获取最新内容）。
		u.mu.RLock()
		ss := u.streamStore
		u.mu.RUnlock()

		result := make(map[uuid.UUID]any, len(messages))
		for id, m := range messages {
			if ss != nil && protocol.MessageStreamingStatus(m.StreamingStatus) == protocol.MessageStreamingStreaming {
				chunks, chunksErr := ss.GetAllChunks(m.ID.String())
				if chunksErr != nil {
					// 降级：返回 DB 原始数据，记录 warn 日志
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
	u.resolvers[string(protocol.EntityTurn)] = func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
		// 分离 nil UUID（占位符）与真实 ID
		var realIDs []uuid.UUID
		for _, id := range ids {
			if id != uuid.Nil {
				realIDs = append(realIDs, id)
			}
		}

		var turns map[uuid.UUID]*model.Turn
		if len(realIDs) > 0 {
			var err error
			turns, err = turnRepo.GetByIDs(ctx, realIDs)
			if err != nil {
				return nil, err
			}
		} else {
			turns = make(map[uuid.UUID]*model.Turn)
		}

		result := make(map[uuid.UUID]any, len(ids))
		for _, id := range ids {
			if id == uuid.Nil {
				result[id] = map[string]any{
					"id":         id.String(),
					"deleted_at": time.Now(),
				}
				continue
			}
			if t, ok := turns[id]; ok {
				result[id] = toProtocolTurn(t)
			}
		}
		return result, nil
	}
	u.resolvers[string(protocol.EntityRtc)] = func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
		// 分离 nil UUID（占位符）与真实 ID
		var realIDs []uuid.UUID
		for _, id := range ids {
			if id != uuid.Nil {
				realIDs = append(realIDs, id)
			}
		}

		var rtcs map[uuid.UUID]*model.Rtc
		if len(realIDs) > 0 {
			var err error
			rtcs, err = rtcRepo.GetByIDs(ctx, realIDs)
			if err != nil {
				return nil, err
			}
		} else {
			rtcs = make(map[uuid.UUID]*model.Rtc)
		}

		result := make(map[uuid.UUID]any, len(ids))
		for _, id := range ids {
			if id == uuid.Nil {
				result[id] = map[string]any{
					"id":         id.String(),
					"deleted_at": time.Now(),
				}
				continue
			}
			if r, ok := rtcs[id]; ok {
				result[id] = toProtocolRtc(r)
			}
		}
		return result, nil
	}

	return u
}

// SetBroker 注入 broker（解决循环依赖：broker 创建时需要 UpdatePublisher 作为 HistoryStore）
func (u *UpdatePublisher) SetBroker(broker Broker) {
	u.mu.Lock()
	u.broker = broker
	u.mu.Unlock()
}

// SetStreamStore 注入 StreamStoreAccessor（用于读取 streaming 状态消息的 chunks）。
// 可选调用：未调用时 streaming 消息返回 DB 原始数据（content 可能不完整）。
func (u *UpdatePublisher) SetStreamStore(s StreamStoreAccessor) {
	u.mu.Lock()
	u.streamStore = s
	u.mu.Unlock()
}

// UpdatePublishItem 一条更新事件，描述一批实体变化
type UpdatePublishItem struct {
	Channel string
	Items   []protocol.UpdateItem
}

// ========== 两阶段发布（推荐） ==========

// Save 在事务内保存 UserUpdate 记录（生成 offset + 写入 DB）。
// 调用方负责管理事务：Begin → WithTx(ctx) → Save → Commit。
func (u *UpdatePublisher) Save(ctx context.Context, items ...UpdatePublishItem) ([]*model.UserUpdate, error) {
	return u.save(ctx, items...)
}

// Push 将已保存的 UserUpdate 转换成富内容并推送到 Centrifuge。
// 应在事务提交之后调用，确保订阅者查询时能看到已提交的数据。
func (u *UpdatePublisher) Push(ctx context.Context, items []UpdatePublishItem, savedUpdates []*model.UserUpdate) ([]*protocol.Update, error) {
	pushUpdates, err := u.convertUpdates(ctx, savedUpdates)
	if err != nil {
		return nil, fmt.Errorf("convert updates: %w", err)
	}
	return u.publishUpdates(ctx, items, pushUpdates)
}

// RunAndPublish 在事务内执行 fn，收集要发布的 UpdatePublishItem，
// 提交后统一推送到 Centrifuge。fn 内的所有 DB 写入应使用传入的 txCtx。
//
// 流程：Begin tx → fn(txCtx) 返回 items → save(items) → commit → push。
// 任何一步失败都会回滚事务并返回错误；commit 成功后 push 失败返回
// ErrPushAfterCommit 包装的错误，调用方通过 errors.Is 检测后可安全忽略
// （数据已持久化，客户端将通过重连同步获取最新状态）。
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

	// 事务提交成功后推送；推送失败通过 ErrPushAfterCommit 告知调用方
	pushUpdates, pushErr := u.Push(ctx, items, saved)
	if pushErr != nil {
		logger.Error(ctx, "[UpdatePublisher] push failed (data already committed)", zap.Error(pushErr))
		return pushUpdates, fmt.Errorf("%w: %v", ErrPushAfterCommit, pushErr)
	}
	return pushUpdates, nil
}

// publishUpdates 把已转好的 []*protocol.Update 按 items 顺序发布到对应频道。
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

			_, err = broker.PublishWithContext(ctx, item.Channel, data, centrifuge.PublishOptions{})
			if err != nil {
				logger.Error(ctx, "[UpdatePublisher] publish to centrifuge failed",
					zap.String("channel", item.Channel),
					zap.String("update_id", update.Id),
					zap.Error(err))
				return nil, fmt.Errorf("publish to centrifuge: %w", err)
			}
			logger.Debug(ctx, "[UpdatePublisher] published update",
				zap.String("channel", item.Channel),
				zap.Uint32("offset", uint32(update.Offset)),
				zap.Int("items", len(update.Items)))
		}
	}
	return pushUpdates, nil
}

// ========== 一体化发布（兼容旧调用） ==========

// Publish 发布更新事件（事务外调用）。
func (u *UpdatePublisher) Publish(ctx context.Context, items ...UpdatePublishItem) ([]*protocol.Update, error) {
	var topicItems, liveItems []UpdatePublishItem
	for _, item := range items {
		if channel.IsTopic(item.Channel) {
			topicItems = append(topicItems, item)
		} else if channel.IsLive(item.Channel) {
			liveItems = append(liveItems, item)
		}
	}

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

func (u *UpdatePublisher) publishTopic(ctx context.Context, items []UpdatePublishItem) ([]*protocol.Update, error) {
	userUpdates, err := u.Save(ctx, items...)
	if err != nil {
		return nil, fmt.Errorf("save updates: %w", err)
	}
	return u.Push(ctx, items, userUpdates)
}

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

func (u *UpdatePublisher) save(ctx context.Context, items ...UpdatePublishItem) ([]*model.UserUpdate, error) {
	channelItemsMap := make(map[string][]UpdatePublishItem)
	for _, item := range items {
		channelItemsMap[item.Channel] = append(channelItemsMap[item.Channel], item)
	}

	var channels []string
	var batchSizes []int
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
			entityUUID, parseErr := uuid.Parse(string(item.EntityId))
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

// buildUpdates 按原始顺序将查询结果回填到 protocol.Update 的 DataList。
func buildUpdates(uus []*model.UserUpdate, allRefs []entityRef, resolved map[string]map[uuid.UUID]any) []*protocol.Update {
	refIdx := 0
	var result []*protocol.Update

	for _, uu := range uus {
		update := &protocol.Update{
			Id:     protocol.UUID(uu.ID.String()),
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
