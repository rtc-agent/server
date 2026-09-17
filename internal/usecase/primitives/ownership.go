// internal/usecase/primitives/ownership.go
package primitives

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"

	"github.com/google/uuid"
)

// CheckSessionOwnership passes through for System Creator; User Creator must
// match the owner. Used by the User workflow for explicit ownership validation
// between "prepare session" and "modify session".
func CheckSessionOwnership(
	ctx context.Context,
	deps *usecase.Dependencies,
	sessionID uuid.UUID,
	creator usecase.Creator,
) error {
	if creator.Kind() == usecase.CreatorKindSystem {
		return nil
	}
	existing, err := deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		if repo.IsNotFound(err) {
			return fmt.Errorf("session %s not found: %w", sessionID, repo.ErrSessionNotFound)
		}
		return fmt.Errorf("get session: %w", err)
	}
	if existing.OwnerKind != string(creator.Kind()) || existing.OwnerRefID != creator.ReferenceID() {
		return fmt.Errorf("session %s: %w", sessionID, repo.ErrPermissionDenied)
	}
	return nil
}

// ResolveTurnSessionID resolves a session ID from a turn ID.
func ResolveTurnSessionID(
	ctx context.Context,
	deps *usecase.Dependencies,
	turnID uuid.UUID,
) (uuid.UUID, error) {
	turn, err := deps.TurnRepo.GetByID(ctx, turnID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("get turn: %w", err)
	}
	return turn.SessionID, nil
}
