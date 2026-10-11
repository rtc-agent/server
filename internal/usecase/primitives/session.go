// internal/usecase/primitives/session.go
package primitives

import (
	"context"
	"errors"
	"fmt"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// PrepareSession reuses an existing session or constructs a new one for creation.
// For existing sessions: only an ownership read-check is performed here; the
// strict closed-state validation is deferred to TouchSession inside the transaction.
// For new sessions: OwnerKind/OwnerRefID are automatically populated from Creator.
func PrepareSession(
	ctx context.Context,
	deps *usecase.Dependencies,
	sessionID *uuid.UUID,
	clientSessionID string,
	creator usecase.Creator,
	initialTitle string,
) (*model.Session, bool, error) {
	if sessionID != nil {
		existing, err := deps.SessionRepo.GetByID(ctx, *sessionID)
		if err != nil {
			if IsNotFound(err) {
				return nil, false, fmt.Errorf("session %s not found: %w", *sessionID, ErrSessionNotFound)
			}
			return nil, false, fmt.Errorf("get session: %w", err)
		}
		// Use internal assertCreatorOwns to avoid redundant DB fetch
		// (CheckSessionOwnership would re-fetch the session we already have).
		if err := assertCreatorOwns(existing, creator); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}

	// New session
	// Idempotency check: if clientSessionID is provided, check if it already exists
	if clientSessionID != "" {
		existing, err := deps.SessionRepo.FindByClientID(ctx, clientSessionID)
		if err != nil {
			return nil, false, fmt.Errorf("find session by client_id %q: %w", clientSessionID, err)
		}
		if existing != nil {
			// Session with this clientSessionID already exists - verify ownership
			if err := assertCreatorOwns(existing, creator); err != nil {
				return nil, false, err
			}
			// Return existing session for idempotency (isNew=false)
			return existing, false, nil
		}
	}

	session := &model.Session{
		ID:         uuid.Must(uuid.NewV7()),
		ClientID:   clientSessionID,
		OwnerKind:  string(creator.Kind()),
		OwnerRefID: creator.ReferenceID(),
		Title:      initialTitle,
		Status:     string(protocol.SessionStatusActive),
	}
	// Backward compatibility: populate DeviceID for user-owned sessions.
	if creator.Kind() == usecase.CreatorKindUser {
		if uc, ok := creator.(usecase.UserCreator); ok {
			session.DeviceID = uc.DeviceID
		}
	}
	return session, true, nil
}

// assertCreatorOwns checks whether the session owner matches the creator.
// Returns ErrPermissionDenied if the creator does not own the session.
func assertCreatorOwns(session *model.Session, creator usecase.Creator) error {
	if session.OwnerKind != string(creator.Kind()) || session.OwnerRefID != creator.ReferenceID() {
		return fmt.Errorf("session %s: %w", session.ID, ErrPermissionDenied)
	}
	return nil
}

// CreateSession creates a session inside a transaction (thin wrapper).
// Handles idempotency: if clientSessionID is provided and a session with that ID
// already exists (race condition), returns the existing session instead of failing.
func CreateSession(txCtx context.Context, deps *usecase.Dependencies, session *model.Session) error {
	err := deps.SessionRepo.Create(txCtx, session)
	if err != nil && session.ClientID != "" && IsDuplicateKeyError(err) {
		// Race condition: another request created a session with the same client_id.
		// Re-query to get the existing session for idempotency.
		existing, findErr := deps.SessionRepo.FindByClientID(txCtx, session.ClientID)
		if findErr != nil {
			return fmt.Errorf("find existing session after conflict: %w", findErr)
		}
		if existing == nil {
			// Should never happen, but handle gracefully
			return fmt.Errorf("session with client_id %s not found after conflict: %w", session.ClientID, err)
		}
		// Copy the existing session data back to the caller's session pointer
		// so they can use it for idempotency.
		*session = *existing
		return nil
	}
	return err
}

// TouchSession touches the updated_at of an active session inside a transaction.
// Returns repo.ErrSessionClosedOrNotFound if the session is closed or does not exist
// (callers use this to determine semantics).
func TouchSession(txCtx context.Context, deps *usecase.Dependencies, sessionID uuid.UUID) error {
	if err := deps.SessionRepo.TouchActive(txCtx, sessionID); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// UpdateSessionFields updates specified fields inside a transaction (only effective
// for active sessions).
func UpdateSessionFields(
	txCtx context.Context,
	deps *usecase.Dependencies,
	sessionID uuid.UUID,
	fields map[string]any,
) error {
	if err := deps.SessionRepo.UpdateFieldsActive(txCtx, sessionID, fields); err != nil {
		if errors.Is(err, ErrSessionClosedOrNotFound) {
			return fmt.Errorf("session %s is closed: %w", sessionID, ErrSessionClosed)
		}
		return fmt.Errorf("update session fields: %w", err)
	}
	return nil
}

// UpdateSessionStatus updates session status inside a transaction.
func UpdateSessionStatus(
	txCtx context.Context,
	deps *usecase.Dependencies,
	sessionID uuid.UUID,
	status protocol.SessionStatus,
) error {
	if err := deps.SessionRepo.UpdateStatus(txCtx, sessionID, status); err != nil {
		return fmt.Errorf("update session status: %w", err)
	}
	return nil
}
