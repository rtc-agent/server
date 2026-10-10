// Package repo provides data access layer (Repository) implementations.
//
// All repo methods must obtain the database handle via DBFromContext to support
// transparent transaction propagation. Sentinel errors are defined centrally
// in errors.go; callers can use errors.Is to determine error types.
package repo

import (
	"context"

	"github.com/rtc-agent/server/internal/dbutil"
	"gorm.io/gorm"
)

// Re-export transaction utilities from dbutil for backwards compatibility.
// New code should import internal/dbutil directly.
var (
	WithTx        = dbutil.WithTx
	TxFromContext = dbutil.TxFromContext
	DBFromContext = dbutil.DBFromContext
)

// Re-export sentinel errors from dbutil for backwards compatibility.
// New code should import internal/dbutil directly.
var (
	// Generic
	ErrNotFound      = dbutil.ErrNotFound
	ErrAlreadyExists = dbutil.ErrAlreadyExists

	// Session
	ErrSessionNotFound         = dbutil.ErrSessionNotFound
	ErrSessionClosed           = dbutil.ErrSessionClosed
	ErrSessionClosedOrNotFound = dbutil.ErrSessionClosedOrNotFound

	// Turn
	ErrTurnNotFound   = dbutil.ErrTurnNotFound
	ErrTurnNotPending = dbutil.ErrTurnNotPending

	// Message
	ErrMessageNotFound = dbutil.ErrMessageNotFound

	// Rtc
	ErrRtcNotFound = dbutil.ErrRtcNotFound

	// OAuth2User
	ErrOAuth2UserNotFound = dbutil.ErrOAuth2UserNotFound

	// Device
	ErrDeviceNotFound = dbutil.ErrDeviceNotFound

	// RefreshToken
	ErrRefreshTokenNotFound = dbutil.ErrRefreshTokenNotFound

	// Goal
	ErrGoalNotFound = dbutil.ErrGoalNotFound

	// Loop
	ErrLoopNotFound = dbutil.ErrLoopNotFound

	// ScriptExecution
	ErrScriptExecutionNotFound = dbutil.ErrScriptExecutionNotFound

	// File (OSS3)
	ErrFileNotFound = dbutil.ErrFileNotFound

	// MultipartUpload (OSS3)
	ErrMultipartUploadNotFound = dbutil.ErrMultipartUploadNotFound

	// Permission
	ErrPermissionDenied = dbutil.ErrPermissionDenied
	ErrPermissionExists = dbutil.ErrPermissionExists

	// Role
	ErrRoleNotFound           = dbutil.ErrRoleNotFound
	ErrRoleNameExists         = dbutil.ErrRoleNameExists
	ErrDuplicateName          = dbutil.ErrDuplicateName
	ErrCannotDeleteSystemRole = dbutil.ErrCannotDeleteSystemRole
	ErrCannotRemoveLastAdmin  = dbutil.ErrCannotRemoveLastAdmin
	ErrCannotRemoveSelfAdmin  = dbutil.ErrCannotRemoveSelfAdmin
	ErrRoleDisabled           = dbutil.ErrRoleDisabled

	// Concurrency
	ErrConflict = dbutil.ErrConflict
)

// Re-export helper functions from dbutil for backwards compatibility.
var (
	IsNotFound          = dbutil.IsNotFound
	IsDuplicateKeyError = dbutil.IsDuplicateKeyError
)

// TxContextKey is exported for tests that need to verify transaction context propagation.
// This is a type alias to dbutil's internal type.
type TxContextKey = txContextKeyType

// txContextKeyType matches dbutil's internal type for type alias compatibility.
type txContextKeyType = struct{}

// WithTxContext is an alias for WithTx for explicit clarity in transaction orchestration code.
func WithTxContext(ctx context.Context, tx *gorm.DB) context.Context {
	return dbutil.WithTx(ctx, tx)
}
