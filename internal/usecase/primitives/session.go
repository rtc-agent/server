// internal/usecase/primitives/session.go
package primitives

import (
	"context"
	"errors"
	"fmt"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
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
			if repo.IsNotFound(err) {
				return nil, false, fmt.Errorf("session %s not found: %w", *sessionID, repo.ErrSessionNotFound)
			}
			return nil, false, fmt.Errorf("get session: %w", err)
		}
		if err := assertCreatorOwns(existing, creator); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}

	// New session
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
func assertCreatorOwns(session *model.Session, creator usecase.Creator) error {
	if session.OwnerKind != string(creator.Kind()) || session.OwnerRefID != creator.ReferenceID() {
		return fmt.Errorf("session %s does not belong to %s/%s",
			session.ID, creator.Kind(), creator.ReferenceID())
	}
	return nil
}

// CreateSession creates a session inside a transaction (thin wrapper).
func CreateSession(txCtx context.Context, deps *usecase.Dependencies, session *model.Session) error {
	return deps.SessionRepo.Create(txCtx, session)
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
		if errors.Is(err, repo.ErrSessionClosedOrNotFound) {
			return fmt.Errorf("session %s is closed: %w", sessionID, repo.ErrSessionClosed)
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
