// internal/handler/rpc/errors.go
package rpchandler

import (
	"context"
	"errors"

	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// internalError logs the full error and returns a generic API error safe for clients.
//
// Used for internal errors such as database, Redis, and transaction failures —
// err.Error() from these may contain table names, SQL statements, connection info,
// and other internal details that must never be exposed to clients.
func (h *Handler) internalError(ctx context.Context, code, publicMsg string, err error) *APIError {
	logger.Error(ctx, publicMsg,
		zap.String("error_code", code),
		zap.Error(err))
	return &APIError{Code: code, Message: publicMsg}
}

// ownershipError classifies errors from CheckSessionOwnership into safe API errors.
//
// CheckSessionOwnership may return three categories of errors:
//   - ErrPermissionDenied: ownership check failed -> "permission_denied"
//   - repo.IsNotFound: session does not exist -> "session.not_found"
//   - Other (database errors, etc.) -> log and return generic "internal_error"
func (h *Handler) ownershipError(ctx context.Context, err error) *APIError {
	if errors.Is(err, repo.ErrPermissionDenied) {
		return &APIError{Code: "permission_denied", Message: "permission denied"}
	}
	if repo.IsNotFound(err) {
		return &APIError{Code: "session.not_found", Message: "session not found"}
	}
	return h.internalError(ctx, "internal_error", "session ownership check failed", err)
}
