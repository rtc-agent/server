package rpchandler

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// parseUUID converts a protocol.UUID to uuid.UUID.
// protocol.UUID is backed by string and is guaranteed valid in production;
// returns an invalid_argument APIError on parse failure.
func parseUUID(id protocol.UUID, fieldName string) (uuid.UUID, *APIError) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, &APIError{
			Code:    "invalid_argument",
			Message: fmt.Sprintf("invalid %s: %s", fieldName, id),
		}
	}
	return parsed, nil
}

// parseUUIDPtr converts a *protocol.UUID to *uuid.UUID. Returns nil for nil input.
func parseUUIDPtr(id *protocol.UUID, fieldName string) (*uuid.UUID, *APIError) {
	if id == nil {
		return nil, nil
	}
	parsed, apiErr := parseUUID(*id, fieldName)
	if apiErr != nil {
		return nil, apiErr
	}
	return &parsed, nil
}

// loadOwnedSession loads a session and verifies ownership by the current user.
// The returned session is guaranteed non-nil; on failure, a wrapped APIError is returned.
func (h *Handler) loadOwnedSession(ctx context.Context, sessionID uuid.UUID, userID uuid.UUID) (*model.Session, error) {
	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{
				Code:    "session.not_found",
				Message: fmt.Sprintf("session %s not found", sessionID),
			}
		}
		return nil, h.internalError(ctx, "session.load_failed", fmt.Sprintf("failed to load session %s", sessionID), err)
	}
	creator := usecase.UserCreator{UserID: userID}
	if session.OwnerKind != string(creator.Kind()) || session.OwnerRefID != creator.ReferenceID() {
		return nil, &APIError{
			Code:    "permission_denied",
			Message: fmt.Sprintf("session %s does not belong to user", sessionID),
		}
	}
	return session, nil
}

// requireUserID extracts the userID from context, returning an unauthorized APIError if missing.
func (h *Handler) requireUserID(ctx context.Context) (uuid.UUID, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return uuid.Nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	return userID, nil
}

// clampLimit validates and normalizes the limit argument: 0 means use default; exceeding max returns an APIError.
func clampLimit(reqLimit *int, defaultLimit, maxLimit int) (int, *APIError) {
	if reqLimit == nil {
		return defaultLimit, nil
	}
	if *reqLimit <= 0 {
		return 0, &APIError{
			Code:    "invalid_argument",
			Message: "limit must be positive",
		}
	}
	if *reqLimit > maxLimit {
		return 0, &APIError{
			Code:    "invalid_argument",
			Message: fmt.Sprintf("limit exceeds maximum (%d)", maxLimit),
		}
	}
	return *reqLimit, nil
}
