package rpchandler

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/infra/context"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// parseUUID 将 protocol.UUID 转换为 uuid.UUID。
// protocol.UUID 底层为 string，线上保证合法；解析失败时返回 invalid_argument APIError。
func parseUUID(id protocol.UUID, fieldName string) (uuid.UUID, *APIError) {
	parsed, err := uuid.Parse(string(id))
	if err != nil {
		return uuid.Nil, &APIError{
			Code:    "invalid_argument",
			Message: fmt.Sprintf("invalid %s: %s", fieldName, id),
		}
	}
	return parsed, nil
}

// parseUUIDPtr 将 *protocol.UUID 转换为 *uuid.UUID。nil 输入返回 nil。
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

// loadOwnedSession 加载 session 并校验归属当前用户。
// 返回的 session 保证非 nil；失败时返回已包装的 APIError。
func (h *Handler) loadOwnedSession(ctx context.Context, sessionID uuid.UUID, userID uuid.UUID) (*model.Session, error) {
	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{
				Code:    "session.not_found",
				Message: fmt.Sprintf("session %s not found", sessionID),
			}
		}
		return nil, &APIError{
			Code:    "session.load_failed",
			Message: fmt.Sprintf("failed to load session %s", sessionID),
		}
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

// requireUserID 从 context 提取 userID，缺失时返回 unauthorized APIError。
func (h *Handler) requireUserID(ctx context.Context) (uuid.UUID, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return uuid.Nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	return userID, nil
}

// clampLimit 校验并规整 limit 参数：0 表示使用默认值，超过 max 时返回 APIError。
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

// derefStr 解引用 *string，nil 返回空字符串。
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
