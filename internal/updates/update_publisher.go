// Package updates 提供 UpdatePublisher 实现，用于将实体变化事件保存并推送到 Centrifuge。
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"gorm.io/gorm"
)

// Broker 发布接口，用于打断 UpdatePublisher 与具体 broker 实现的循环依赖。
type Broker interface {
	PublishWithContext(ctx context.Context, channel string, data []byte, opts centrifuge.PublishOptions) (centrifuge.PublishResult, error)
}

// EntityResolver 根据实体类型和 ID 查询富内容。
type EntityResolver func(ctx context.Context, id uuid.UUID) (any, error)

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

	// 注册实体解析器（registry 模式，替代大 switch）
	u.resolvers[string(protocol.EntitySession)] = func(ctx context.Context, id uuid.UUID) (any, error) {
		s, err := sessionRepo.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		return toProtocolSession(s), nil
	}
	u.resolvers[string(protocol.EntityMessage)] = func(ctx context.Context, id uuid.UUID) (any, error) {
		m, err := messageRepo.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}

		// 流式消息：从 Redis 读取 chunks 拼接到 content。
		// 竞争条件：chunks 可能未完全写入，接受最终一致性（前端通过后续 update 获取最新内容）。
		if protocol.MessageStreamingStatus(m.StreamingStatus) == protocol.MessageStreamingStreaming && u.streamStore != nil {
			chunks, chunksErr := u.streamStore.GetAllChunks(m.ID.String())
			if chunksErr != nil {
				// 降级：返回 DB 原始数据，记录 warn 日志
				logger.Warn(ctx, "[UpdatePublisher] get chunks for streaming message failed, falling back to DB data",
					zap.String("message_id", m.ID.String()),
					zap.Error(chunksErr),
				)
			} else if len(chunks) > 0 {
				v := &protocol.ContentData{}
				e := json.Unmarshal([]byte(m.Content), v)
				if e != nil {
					logger.Warn(ctx, "[UpdatePublisher] unmarshal content failed", zap.Error(e))
					m.Content = strings.Join(chunks, "")
				} else {
					v.Data = strings.Join(chunks, "")
					tmp, _ := json.Marshal(v)
					m.Content = string(tmp)
				}
			}
		}

		return toProtocolMessage(m), nil
	}
	u.resolvers[string(protocol.EntityTurn)] = func(ctx context.Context, id uuid.UUID) (any, error) {
		// Skip zero UUID to prevent invalid DB queries - return placeholder
		if id == uuid.Nil {
			return map[string]any{
				"id":         id.String(),
				"deleted_at": time.Now(),
			}, nil
		}
		t, err := turnRepo.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		return toProtocolTurn(t), nil
	}
	u.resolvers[string(protocol.EntityRtc)] = func(ctx context.Context, id uuid.UUID) (any, error) {
		// Skip zero UUID to prevent invalid DB queries - return placeholder
		if id == uuid.Nil {
			return map[string]any{
				"id":         id.String(),
				"deleted_at": time.Now(),
			}, nil
		}
		r, err := rtcRepo.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		return toProtocolRtc(r), nil
	}

	return u
}

// SetBroker 注入 broker（解决循环依赖：broker 创建时需要 UpdatePublisher 作为 HistoryStore）
func (u *UpdatePublisher) SetBroker(broker Broker) {
	u.broker = broker
}

// SetStreamStore 注入 StreamStoreAccessor（用于读取 streaming 状态消息的 chunks）。
// 可选调用：未调用时 streaming 消息返回 DB 原始数据（content 可能不完整）。
func (u *UpdatePublisher) SetStreamStore(s StreamStoreAccessor) {
	u.streamStore = s
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
// 任何一步失败都会回滚事务并返回错误；commit 成功后 push 失败只记录日志，
// 不影响业务结果（数据已持久化）。
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

	// 事务提交成功后推送；推送失败只记录，不回滚
	pushUpdates, pushErr := u.Push(ctx, items, saved)
	if pushErr != nil {
		logger.Error(ctx, "[UpdatePublisher] push failed (data already committed)", zap.Error(pushErr))
	}
	return pushUpdates, nil
}

// publishUpdates 把已转好的 []*protocol.Update 按 items 顺序发布到对应频道。
func (u *UpdatePublisher) publishUpdates(ctx context.Context, items []UpdatePublishItem, pushUpdates []*protocol.Update) ([]*protocol.Update, error) {
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

			_, err = u.broker.PublishWithContext(ctx, item.Channel, data, centrifuge.PublishOptions{})
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

			_, err = u.broker.PublishWithContext(ctx, item.Channel, data, centrifuge.PublishOptions{})
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
	for ch, channelItems := range channelItemsMap {
		channels = append(channels, ch)
		batchSizes = append(batchSizes, len(channelItems))
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
// 使用 registry 模式分派实体查询，替代硬编码 switch。
func (u *UpdatePublisher) convertUpdates(ctx context.Context, uus []*model.UserUpdate) ([]*protocol.Update, error) {
	var result []*protocol.Update

	for _, uu := range uus {
		update := &protocol.Update{
			Id:     protocol.UUID(uu.ID.String()),
			Items:  make([]protocol.UpdateItem, len(uu.Items)),
			Offset: uu.Offset,
		}

		// 复制 items 引用
		copy(update.Items, uu.Items)

		// 根据 items 查询富内容
		var dataList []any
		for _, item := range uu.Items {
			resolver, ok := u.resolvers[string(item.Entity)]
			if !ok {
				// 未知实体类型，跳过富内容
				dataList = append(dataList, nil)
				continue
			}

			entityUUID, parseErr := uuid.Parse(string(item.EntityId))
			if parseErr != nil {
				return nil, fmt.Errorf("parse entity ID %s: %w", item.EntityId, parseErr)
			}
			data, err := resolver(ctx, entityUUID)
			if err != nil {
				if repo.IsNotFound(err) {
					// 实体已删除，返回带 DeletedAt 的占位数据
					now := time.Now()
					dataList = append(dataList, map[string]any{
						"id":         item.EntityId,
						"deleted_at": now,
					})
					continue
				}
				return nil, fmt.Errorf("resolve entity %s/%s: %w", item.Entity, item.EntityId, err)
			}
			dataList = append(dataList, data)
		}

		update.DataList = &dataList
		result = append(result, update)
	}

	return result, nil
}
