package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
)

// goalTerminalStatuses 定义 Goal 的终态集合
var goalTerminalStatuses = []any{
	model.GoalStatusCompleted,
	model.GoalStatusCancelled,
	model.GoalStatusExhausted,
}

// loopTerminalStatuses 定义 Loop 的终态集合
var loopTerminalStatuses = []any{
	model.LoopStatusCompleted,
	model.LoopStatusCancelled,
	model.LoopStatusExhausted,
}

// autoFillCompletedAt 在终态时自动填充 completed_at 字段。
// 当 fields 中包含 status 且值为终态之一，且 completed_at 未被显式设置时，
// 自动将 completed_at 设为当前时间。
func autoFillCompletedAt(fields map[string]any, terminalStatuses []any) {
	if _, ok := fields["status"]; ok {
		statusStr := fmt.Sprintf("%v", fields["status"])
		for _, ts := range terminalStatuses {
			if statusStr == fmt.Sprintf("%v", ts) && fields["completed_at"] == nil {
				fields["completed_at"] = time.Now()
				return
			}
		}
	}
}

// listBySessionPaged 通用的按 session 游标分页查询。
// orderClause: 排序子句，如 "created_at DESC, id DESC"
// cursorCol/cursorOp: 游标条件列和操作符，如 "id", "<"
func listBySessionPaged[T any](
	ctx context.Context,
	db *gorm.DB,
	sessionID uuid.UUID,
	cursor *string,
	limit int,
	orderClause string,
	cursorCol string,
	cursorOp string,
	entityName string,
) ([]*T, error) {
	var items []*T
	q := DBFromContext(ctx, db).WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order(orderClause)
	if cursor != nil {
		q = q.Where(cursorCol+" "+cursorOp+" ?", *cursor)
	}
	if limit <= 0 {
		limit = 50
	}
	if err := q.Limit(limit).Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list %s for session %s: %w", entityName, sessionID, err)
	}
	return items, nil
}

// getByIDs 通用的按 ID 批量查询，返回 map[uuid.UUID]*T。
// getID 从实体中提取 ID 字段。
func getByIDs[T any](
	ctx context.Context,
	db *gorm.DB,
	ids []uuid.UUID,
	getID func(*T) uuid.UUID,
	entityName string,
) (map[uuid.UUID]*T, error) {
	if len(ids) == 0 {
		return make(map[uuid.UUID]*T), nil
	}
	var items []*T
	if err := DBFromContext(ctx, db).WithContext(ctx).Where("id IN ?", ids).Find(&items).Error; err != nil {
		return nil, fmt.Errorf("get %s by ids: %w", entityName, err)
	}
	result := make(map[uuid.UUID]*T, len(items))
	for _, item := range items {
		result[getID(item)] = item
	}
	return result, nil
}

// updateWithAutoTimestamp 通用的按 ID 更新，自动设置 updated_at。
// model: GORM Model 实例（如 &model.Session{}）
// whereClause: WHERE 子句模板，如 "id = ?" 或 "id = ? AND deleted_at IS NULL"
func updateWithAutoTimestamp(
	ctx context.Context,
	db *gorm.DB,
	model any,
	id uuid.UUID,
	fields map[string]any,
	whereClause string,
	entityName string,
	notFoundErr error,
) error {
	// Copy to avoid mutating the caller's map.
	updates := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		updates[k] = v
	}
	updates["updated_at"] = time.Now()

	result := DBFromContext(ctx, db).WithContext(ctx).
		Model(model).
		Where(whereClause, id).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("update %s %s: %w", entityName, id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update %s %s: %w", entityName, id, notFoundErr)
	}
	return nil
}

// listByCategory 通用的按分类查询。
// scopeCol: 范围列名，如 "session_id" 或 "user_id"
// orderClause: 排序子句，如 "created_at DESC"
// extraWhere: 额外的 WHERE 条件，如 "AND deleted_at IS NULL"，为空则不加
func listByCategory[T any](
	ctx context.Context,
	db *gorm.DB,
	scopeCol string,
	scopeID uuid.UUID,
	category string,
	limit int,
	orderClause string,
	extraWhere string,
	entityName string,
) ([]*T, error) {
	if limit <= 0 {
		limit = 20
	}
	var items []*T
	where := scopeCol + " = ? AND category = ?"
	if extraWhere != "" {
		where += " " + extraWhere
	}
	if err := DBFromContext(ctx, db).WithContext(ctx).
		Where(where, scopeID, category).
		Order(orderClause).
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list %s by category %s: %w", entityName, category, err)
	}
	return items, nil
}
