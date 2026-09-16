package rpchandler

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// getOwnedByID is a generic helper for the common "get entity by ID, verify
// session ownership" pattern used across query handlers.
//
// Parameters:
//   - ctx: request context
//   - idStr: the string representation of the entity UUID
//   - idField: the JSON field name for error messages (e.g., "message_id")
//   - errorCode: the error code for API errors (e.g., "message.not_found")
//   - entityName: human-readable entity name for log messages (e.g., "message")
//   - getByID: function to fetch the entity from the repository
//   - getSessionID: function to extract the session ID from the entity
//
// Returns the entity, the authenticated user ID, or an APIError.
func (h *Handler) getOwnedByID[T any](
	ctx context.Context,
	idStr string,
	idField string,
	errorCode string,
	entityName string,
	getByID func(ctx context.Context, id uuid.UUID) (*T, error),
	getSessionID func(entity *T) uuid.UUID,
) (*T, uuid.UUID, error) {
	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, uuid.Nil, err
	}

	entityUUID, apiErr := parseUUID(idStr, idField)
	if apiErr != nil {
		return nil, uuid.Nil, apiErr
	}

	entity, err := getByID(ctx, entityUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, uuid.Nil, &APIError{
				Code:    errorCode,
				Message: fmt.Sprintf("%s %s not found", entityName, idStr),
			}
		}
		return nil, uuid.Nil, h.internalError(ctx, entityName+".error", "internal error", err)
	}

	sessionID := getSessionID(entity)
	if _, err := h.loadOwnedSession(ctx, sessionID, userID); err != nil {
		return nil, uuid.Nil, err
	}

	logger.Info(ctx, "["+entityName+".get]",
		zap.String("user", userID.String()),
		zap.String(entityName, idStr))

	return entity, userID, nil
}
