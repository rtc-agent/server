package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"

	"gorm.io/gorm"
)

// MessageRepo 消息仓储接口
type MessageRepo interface {
	// Create 创建新消息记录
	Create(ctx context.Context, msg *model.Message) error
	BatchCreate(ctx context.Context, messages []*model.Message) error
	// GetByID 根据 ID 查询消息
	GetByID(ctx context.Context, id uuid.UUID) (*model.Message, error)
	// FindByClientID 根据 clientID 查询消息
	FindByClientID(ctx context.Context, clientID string) (*model.Message, error)
	// ListBySession 按 session 分页查询消息列表
	ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *uint32, limit int) ([]*model.Message, error)
	// ListRecentBySession returns the most recent `limit` messages for a session,
	// ordered by global_offset ASC (oldest first within the returned set).
	// Unlike ListBySession which returns the oldest messages (ASC + LIMIT from start),
	// this returns the newest messages (DESC + LIMIT, then reversed to ASC).
	ListRecentBySession(ctx context.Context, sessionID uuid.UUID, limit int) ([]*model.Message, error)
	// ListBySessionBeforeOffset returns the most recent `limit` messages with global_offset <= maxOffset,
	// ordered by global_offset ASC (oldest first).
	ListBySessionBeforeOffset(ctx context.Context, sessionID uuid.UUID, maxOffset uint32, limit int) ([]*model.Message, error)
	// GetNextGlobalOffset 获取 session 下一个全局偏移量
	GetNextGlobalOffset(ctx context.Context, sessionID uuid.UUID) (uint32, error)
	// UpdateStreamingStatus 更新消息流式状态及内容
	UpdateStreamingStatus(ctx context.Context, id uuid.UUID, status protocol.MessageStreamingStatus, content string) error
	// DeleteByIDs hard-deletes messages by their IDs.
	DeleteByIDs(ctx context.Context, ids []uuid.UUID) error
	// GetByIDs 批量查询消息，返回 map[id]*Message。未找到的 ID 不会出现在 map 中。
	GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Message, error)
	// UpdateTokenUsage updates the token usage fields for a message.
	// Used to record LLM token usage on assistant messages after stream finalization.
	UpdateTokenUsage(ctx context.Context, id uuid.UUID, usage *model.TokenUsageUpdate) error
}

type messageRepo struct {
	db *gorm.DB
}

// NewMessageRepo 创建 MessageRepo
func NewMessageRepo(db *gorm.DB) MessageRepo {
	return &messageRepo{db: db}
}

func (r *messageRepo) Create(ctx context.Context, msg *model.Message) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(msg).Error; err != nil {
		return fmt.Errorf("create message: %w", err)
	}
	return nil
}

// BatchCreate 批量创建消息（一次 INSERT）。
func (r *messageRepo) BatchCreate(ctx context.Context, messages []*model.Message) error {
	if len(messages) == 0 {
		return nil
	}
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(&messages).Error; err != nil {
		return fmt.Errorf("batch create messages: %w", err)
	}
	return nil
}

func (r *messageRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Message, error) {
	var msg model.Message
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&msg, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get message %s: %w", id, ErrMessageNotFound)
		}
		return nil, fmt.Errorf("get message %s: %w", id, err)
	}
	return &msg, nil
}

func (r *messageRepo) FindByClientID(ctx context.Context, clientID string) (*model.Message, error) {
	if clientID == "" {
		return nil, nil
	}
	var msg model.Message
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&msg, "client_id = ?", clientID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil // Not found is not an error
		}
		return nil, fmt.Errorf("find message by client_id %s: %w", clientID, err)
	}
	return &msg, nil
}

func (r *messageRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *uint32, limit int) ([]*model.Message, error) {
	var messages []*model.Message
	q := DBFromContext(ctx, r.db).WithContext(ctx).Where("session_id = ?", sessionID).Order("global_offset ASC")
	if cursor != nil {
		q = q.Where("global_offset > ?", *cursor)
	}
	if limit <= 0 {
		limit = 50
	}
	if err := q.Limit(limit).Find(&messages).Error; err != nil {
		return nil, fmt.Errorf("list messages by session %s: %w", sessionID, err)
	}
	return messages, nil
}

func (r *messageRepo) GetNextGlobalOffset(ctx context.Context, sessionID uuid.UUID) (uint32, error) {
	var maxOffset uint32
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Message{}).
		Where("session_id = ?", sessionID).
		Select("COALESCE(MAX(global_offset), 0)").
		Scan(&maxOffset).Error
	if err != nil {
		return 0, fmt.Errorf("get next global offset for session %s: %w", sessionID, err)
	}
	return maxOffset + 1, nil
}

func (r *messageRepo) UpdateStreamingStatus(ctx context.Context, id uuid.UUID, status protocol.MessageStreamingStatus, content string) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Message{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"streaming_status": string(status),
			"content":          content,
		})
	if result.Error != nil {
		return fmt.Errorf("update message %s streaming status: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update message %s streaming status: %w", id, ErrMessageNotFound)
	}
	return nil
}

// ListRecentBySession returns the most recent `limit` messages for a session.
// It queries with ORDER BY global_offset DESC LIMIT N, then reverses the result
// so the returned slice is in chronological order (oldest first).
func (r *messageRepo) ListRecentBySession(ctx context.Context, sessionID uuid.UUID, limit int) ([]*model.Message, error) {
	if limit <= 0 {
		limit = 50
	}
	var messages []*model.Message
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("global_offset DESC").
		Limit(limit).
		Find(&messages).Error; err != nil {
		return nil, fmt.Errorf("list recent messages by session %s: %w", sessionID, err)
	}
	// Reverse to chronological order (oldest first)
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}

// DeleteByIDs hard-deletes messages by their IDs.
func (r *messageRepo) DeleteByIDs(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("id IN ?", ids).
		Delete(&model.Message{}).Error; err != nil {
		return fmt.Errorf("delete messages %v: %w", ids, err)
	}
	return nil
}

// ListBySessionBeforeOffset returns the most recent `limit` messages with global_offset <= maxOffset.
// Results are ordered by global_offset ASC (oldest first).
func (r *messageRepo) ListBySessionBeforeOffset(ctx context.Context, sessionID uuid.UUID, maxOffset uint32, limit int) ([]*model.Message, error) {
	if limit <= 0 {
		limit = 50
	}
	// 先 DESC 查询最新的 limit 条
	var messages []*model.Message
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ? AND global_offset <= ?", sessionID, maxOffset).
		Order("global_offset DESC").
		Limit(limit).
		Find(&messages).Error; err != nil {
		return nil, fmt.Errorf("list messages before offset %d for session %s: %w", maxOffset, sessionID, err)
	}
	// 反转为 ASC 顺序（旧的在前）
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}

func (r *messageRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Message, error) {
	return getByIDs[model.Message](ctx, r.db, ids, func(m *model.Message) uuid.UUID { return m.ID }, "messages")
}

func (r *messageRepo) UpdateTokenUsage(ctx context.Context, id uuid.UUID, usage *model.TokenUsageUpdate) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Message{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"input_tokens":     usage.InputTokens,
			"output_tokens":    usage.OutputTokens,
			"total_tokens":     usage.TotalTokens,
			"cached_tokens":    usage.CachedTokens,
			"reasoning_tokens": usage.ReasoningTokens,
		})
	if result.Error != nil {
		return fmt.Errorf("update message %s token usage: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update message %s token usage: %w", id, ErrMessageNotFound)
	}
	return nil
}
