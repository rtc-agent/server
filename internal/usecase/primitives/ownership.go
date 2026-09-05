// internal/usecase/primitives/ownership.go
package primitives

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"

	"github.com/google/uuid"
)

// CheckSessionOwnership 系统 Creator 直接放行；User Creator 必须 owner 匹配。
// 供 User workflow 在"准备 session"之后、"修改 session"之前的显式归属校验使用。
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

// ResolveTurnSessionID 通过 turn ID 反查 session ID。
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
