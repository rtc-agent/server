// internal/usecase/primitives/ownership.go
package primitives

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CheckSessionOwnership passes through for System Creator; User Creator must
// match the owner. Used by the User workflow for explicit ownership validation
// between "prepare session" and "modify session".
// Logs detailed information on permission denial for debugging and audit purposes.
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
		if IsNotFound(err) {
			// Log not-found for security audit (potential unauthorized access attempts)
			logger.Warn(ctx, "permission_check.session_not_found",
				zap.String("session_id", sessionID.String()),
				zap.String("creator_kind", string(creator.Kind())),
				zap.String("creator_ref_id", creator.ReferenceID()),
			)
			return fmt.Errorf("session %s not found: %w", sessionID, ErrSessionNotFound)
		}
		return fmt.Errorf("get session: %w", err)
	}
	// Delegate to shared helper to avoid logic duplication with PrepareSession.
	return assertCreatorOwns(existing, creator)
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
