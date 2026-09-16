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

// listBySessionCursor is a generic helper for cursor-paginated list handlers
// that share the common auth + session ownership verification pattern.
//
// Parameters:
//   - ctx: request context
//   - sessionID: raw session ID string from the request
//   - limit: raw limit from the request
//   - cursor: cursor value from the request
//   - entityName: entity name for log/error messages (e.g., "rtc", "turn")
//   - list: function to fetch items from the repository
//   - toProtocol: function to convert a domain model item to its protocol representation
//   - getEntityID: function to extract the string ID from a domain model item (for nextCursor)
func listBySessionCursor[T any, P any](
	ctx context.Context,
	h *Handler,
	sessionID string,
	reqLimit *int,
	cursor *string,
	entityName string,
	list func(ctx context.Context, sessionUUID uuid.UUID, cursor *string, limit int) ([]*T, error),
	toProtocol func(*T) P,
	getEntityID func(*T) string,
) ([]P, *string, error) {
	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, nil, err
	}

	clampedLimit, apiErr := clampLimit(reqLimit, h.deps.API.QueryDefaultLimit, h.deps.API.QueryMaxLimit)
	if apiErr != nil {
		return nil, nil, apiErr
	}

	sessionUUID, apiErr := parseUUID(sessionID, "session_id")
	if apiErr != nil {
		return nil, nil, apiErr
	}

	if _, err := h.loadOwnedSession(ctx, sessionUUID, userID); err != nil {
		return nil, nil, err
	}

	items, err := list(ctx, sessionUUID, cursor, clampedLimit)
	if err != nil {
		return nil, nil, h.internalError(ctx, entityName+".list_failed", "internal error", err)
	}

	protoItems := make([]P, 0, len(items))
	for _, item := range items {
		protoItems = append(protoItems, toProtocol(item))
	}

	var nextCursor *string
	if len(items) == clampedLimit {
		last := getEntityID(items[len(items)-1])
		nextCursor = &last
	}

	logger.Info(ctx, "["+entityName+".list]",
		zap.String("user", userID.String()),
		zap.String("session", sessionID),
		zap.Int("count", len(protoItems)),
		zap.Bool("has_next", nextCursor != nil))

	return protoItems, nextCursor, nil
}
