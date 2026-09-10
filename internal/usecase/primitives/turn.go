// internal/usecase/primitives/turn.go
package primitives

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"

	"github.com/google/uuid"
	"go.uber.org/zap"
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

// StopActiveTurns stops all pending/running/interrupted turns for a session.
//
// Steps:
//  1. Cancel all pending/processing work via rtc-queue CancelSession FIRST.
//     This must happen before the DB update so the cancel signal reaches the
//     worker while the turn is still "active" in the DB. If we updated the DB
//     first, the frontend would see "cancelled" while the LLM is still running
//     (because the cancel Pub/Sub message may arrive late or be lost entirely
//     due to a race between PUBLISH and the worker's SUBSCRIBE).
//  2. Query active turns BEFORE the bulk DB update (so we have turn IDs to
//     publish events for).
//  3. Mark remaining active turns as cancelled in the DB (belt-and-suspenders
//     for races where a turn was created but not yet published to rtc-queue).
//  4. Publish turn.updated events for each cancelled turn so the frontend
//     learns that the turns are no longer active.
func StopActiveTurns(ctx context.Context, deps *usecase.Dependencies, queue *rtcqueue.Queue, sessionID uuid.UUID, reason string) {
	// 1. Cancel all pending/processing work via rtc-queue FIRST.
	// This sends the cancel signal to the worker before we mark the turns as
	// cancelled in the DB. The DB update below is a safety net for turns that
	// were created but not yet claimed by a worker.
	if queue != nil {
		if err := queue.CancelSession(ctx, sessionID.String(), reason); err != nil {
			logger.Error(ctx, "[StopActiveTurns] cancel session failed",
				zap.String("session", sessionID.String()), zap.Error(err))
		}
	}

	// 2. Query active turns BEFORE updating (so we can publish events for each).
	activeTurns, err := deps.TurnRepo.FindActiveBySession(ctx, sessionID)
	if err != nil {
		logger.Error(ctx, "[StopActiveTurns] find active turns failed",
			zap.String("session", sessionID.String()), zap.Error(err))
		return
	}

	// 3. Mark remaining active turns as cancelled in DB.
	// After step 1, the worker has already received the cancel signal (if the
	// Pub/Sub message was delivered). This DB update is a safety net for turns
	// that were created but not yet claimed by a worker, or for cases where
	// the cancel Pub/Sub message was lost.
	if affected, err := deps.TurnRepo.UpdateStatusBySession(
		ctx, sessionID,
		[]string{
			string(model.TurnStatusPending),
			string(model.TurnStatusRunning),
			string(model.TurnStatusInterrupted),
		},
		protocol.TurnStatusCancelled,
	); err != nil {
		logger.Error(ctx, "[StopActiveTurns] update turns status failed",
			zap.String("session", sessionID.String()), zap.Error(err))
	} else if affected > 0 {
		logger.Info(ctx, "[StopActiveTurns] cancelled turns",
			zap.Int("affected", int(affected)), zap.String("session", sessionID.String()))
	}

	// 4. Publish turn.updated events for all cancelled turns in a single batch.
	if len(activeTurns) == 0 || deps.UpdatePublisher == nil {
		return
	}

	session, sessErr := deps.SessionRepo.GetByID(ctx, sessionID)
	if sessErr != nil {
		logger.Error(ctx, "[StopActiveTurns] load session failed",
			zap.String("session", sessionID.String()), zap.Error(sessErr))
		return
	}

	var allItems []protocol.UpdateItem
	for _, turn := range activeTurns {
		turnUpdates := BuildTurnUpdatedUpdates(session, turn.ID)
		for _, u := range turnUpdates {
			allItems = append(allItems, u.Items...)
		}
	}
	if len(allItems) == 0 {
		return
	}

	ch := channel.UserTopic(session.OwnerRefID)
	merged := []updates.UpdatePublishItem{{Channel: ch, Items: allItems}}
	if _, err := deps.UpdatePublisher.Publish(ctx, merged...); err != nil {
		logger.Error(ctx, "[StopActiveTurns] batch publish turn updates failed",
			zap.String("session", sessionID.String()), zap.Int("turn_count", len(activeTurns)), zap.Error(err))
	}
}
