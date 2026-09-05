// internal/handler/rpc/errors.go
package rpchandler

import (
	"context"
	"errors"

	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// internalError 记录完整错误到日志，返回对客户端安全的通用 API 错误。
//
// 用于数据库、Redis、事务等内部错误——这些错误的 err.Error() 可能包含
// 表名、SQL 语句、连接信息等内部细节，绝不应该暴露给客户端。
func (h *Handler) internalError(ctx context.Context, code, publicMsg string, err error) *APIError {
	logger.Error(ctx, publicMsg,
		zap.String("error_code", code),
		zap.Error(err))
	return &APIError{Code: code, Message: publicMsg}
}

// ownershipError 将 CheckSessionOwnership 返回的错误分类为安全的 API 错误。
//
// CheckSessionOwnership 可能返回三类错误：
//   - ErrPermissionDenied：归属校验失败 → "permission_denied"
//   - repo.IsNotFound：session 不存在 → "session.not_found"
//   - 其他（数据库错误等） → 记录日志，返回通用 "internal_error"
func (h *Handler) ownershipError(ctx context.Context, err error) *APIError {
	if errors.Is(err, repo.ErrPermissionDenied) {
		return &APIError{Code: "permission_denied", Message: "permission denied"}
	}
	if repo.IsNotFound(err) {
		return &APIError{Code: "session.not_found", Message: "session not found"}
	}
	return h.internalError(ctx, "internal_error", "session ownership check failed", err)
}
