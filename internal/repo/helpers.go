package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
)

// goalTerminalStatuses defines the set of terminal Goal statuses.
var goalTerminalStatuses = []any{
	model.GoalStatusCompleted,
	model.GoalStatusCancelled,
	model.GoalStatusExhausted,
}

// loopTerminalStatuses defines the set of terminal Loop statuses.
var loopTerminalStatuses = []any{
	model.LoopStatusCompleted,
	model.LoopStatusCancelled,
	model.LoopStatusExhausted,
}

// autoFillCompletedAt automatically sets completed_at when transitioning to a
// terminal status. If fields contains "status" with a terminal value and
// completed_at is not explicitly set, it is set to the current time.
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

// listBySessionPaged is a generic cursor-paginated query scoped to a session.
// orderClause: ORDER BY clause, e.g. "created_at DESC, id DESC".
// cursorCol/cursorOp: cursor column and operator, e.g. "id", "<".
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

// getByIDs is a generic batch lookup by IDs, returning map[uuid.UUID]*T.
// getID extracts the ID field from the entity.
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

// updateWithAutoTimestamp is a generic update-by-ID that auto-sets updated_at.
// model: GORM model instance (e.g. &model.Session{}).
// whereClause: WHERE clause template, e.g. "id = ?".
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

// listByCategory is a generic category-scoped query.
// scopeCol: scope column name, e.g. "session_id" or "user_id".
// orderClause: ORDER BY clause, e.g. "created_at DESC".
// extraWhere: additional WHERE conditions, e.g. "AND deleted_at IS NULL"; empty to skip.
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
