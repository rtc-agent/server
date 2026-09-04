// internal/usecase/primitives/turn.go
package primitives

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// CreateTurn 在事务内创建 turn（thin wrapper）。
func CreateTurn(txCtx context.Context, deps *usecase.Dependencies, turn *model.Turn) error {
	return deps.TurnRepo.Create(txCtx, turn)
}

// UpdateTurnStatus 在事务内更新 turn 状态。
func UpdateTurnStatus(
	txCtx context.Context,
	deps *usecase.Dependencies,
	turnID uuid.UUID,
	status protocol.TurnStatus,
	errMsg string,
) error {
	if err := deps.TurnRepo.UpdateStatus(txCtx, turnID, status, errMsg); err != nil {
		return fmt.Errorf("update turn status: %w", err)
	}
	return nil
}
