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

// PrepareSession 复用已有 session 或构造待创建的新 session。
// 对于已有 session：此处仅做归属读校验；closed 状态的严格判断推迟到事务内 TouchSession。
// 对于新 session：OwnerKind/OwnerRefID 自动按 Creator 填充。
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

	// 新建
	session := &model.Session{
		ID:         uuid.Must(uuid.NewV7()),
		ClientID:   clientSessionID,
		OwnerKind:  string(creator.Kind()),
		OwnerRefID: creator.ReferenceID(),
		Title:      initialTitle,
		Status:     string(protocol.SessionStatusActive),
	}
	// 向后兼容：user-owned session 填充 DeviceID
	if creator.Kind() == usecase.CreatorKindUser {
		if uc, ok := creator.(usecase.UserCreator); ok {
			session.DeviceID = uc.DeviceID
		}
	}
	return session, true, nil
}

// assertCreatorOwns 检查 session 的 owner 与 creator 是否匹配。
func assertCreatorOwns(session *model.Session, creator usecase.Creator) error {
	if session.OwnerKind != string(creator.Kind()) || session.OwnerRefID != creator.ReferenceID() {
		return fmt.Errorf("session %s does not belong to %s/%s",
			session.ID, creator.Kind(), creator.ReferenceID())
	}
	return nil
}

// CreateSession 在事务内创建 session（thin wrapper）。
func CreateSession(txCtx context.Context, deps *usecase.Dependencies, session *model.Session) error {
	return deps.SessionRepo.Create(txCtx, session)
}

// TouchSession 在事务内 touch 活跃 session 的 updated_at。
// 若 session 已关闭或不存在，返回 repo.ErrSessionClosedOrNotFound（调用方据此判断语义）。
func TouchSession(txCtx context.Context, deps *usecase.Dependencies, sessionID uuid.UUID) error {
	if err := deps.SessionRepo.TouchActive(txCtx, sessionID); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// UpdateSessionFields 在事务内更新指定字段（仅对 active session 生效）。
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

// UpdateSessionStatus 在事务内更新 session 状态。
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
