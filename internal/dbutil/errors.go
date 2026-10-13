package dbutil

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Sentinel errors for callers to check via errors.Is.
//
// These are shared infrastructure errors used across the codebase.
// Domain-specific repos may define more specific errors that wrap these.
var (
	// Generic
	ErrNotFound      = errors.New("record not found")
	ErrAlreadyExists = errors.New("record already exists")

	// Session
	ErrSessionNotFound         = errors.New("session not found")
	ErrSessionClosed           = errors.New("session is closed")
	ErrSessionClosedOrNotFound = errors.New("session is closed or not found")

	// Turn
	ErrTurnNotFound   = errors.New("turn not found")
	ErrTurnNotPending = errors.New("turn is not in pending status")

	// Message
	ErrMessageNotFound = errors.New("message not found")

	// Rtc
	ErrRtcNotFound = errors.New("rtc not found")

	// OAuth2User
	ErrOAuth2UserNotFound = errors.New("oauth2 user not found")

	// Device
	ErrDeviceNotFound = errors.New("device not found")

	// RefreshToken
	ErrRefreshTokenNotFound = errors.New("refresh token not found")

	// Goal
	ErrGoalNotFound = errors.New("goal not found")

	// Loop
	ErrLoopNotFound = errors.New("loop not found")

	// ScriptExecution
	ErrScriptExecutionNotFound = errors.New("script execution not found")

	// File (OSS3)
	ErrFileNotFound = errors.New("file not found")

	// MultipartUpload (OSS3)
	ErrMultipartUploadNotFound = errors.New("multipart upload not found")

	// Permission
	ErrPermissionDenied = errors.New("permission denied")
	ErrPermissionExists = errors.New("permission policy already exists")

	// Role
	ErrRoleNotFound           = errors.New("role not found")
	ErrRoleNameExists         = errors.New("role name already exists")
	ErrDuplicateName          = ErrRoleNameExists // alias for ErrRoleNameExists per spec
	ErrCannotDeleteSystemRole = errors.New("cannot delete system role")
	ErrCannotRemoveLastAdmin  = errors.New("cannot remove the last admin role assignment")
	ErrCannotRemoveSelfAdmin  = errors.New("cannot remove your own admin role")
	ErrRoleDisabled           = errors.New("role is disabled")

	// Concurrency
	ErrConflict = errors.New("optimistic lock conflict")
)

// IsNotFound checks whether the error is a "not found" variant.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrSessionNotFound) ||
		errors.Is(err, ErrTurnNotFound) ||
		errors.Is(err, ErrMessageNotFound) ||
		errors.Is(err, ErrRtcNotFound) ||
		errors.Is(err, ErrOAuth2UserNotFound) ||
		errors.Is(err, ErrDeviceNotFound) ||
		errors.Is(err, ErrRefreshTokenNotFound) ||
		errors.Is(err, ErrGoalNotFound) ||
		errors.Is(err, ErrLoopNotFound) ||
		errors.Is(err, ErrScriptExecutionNotFound) ||
		errors.Is(err, ErrFileNotFound) ||
		errors.Is(err, ErrMultipartUploadNotFound) ||
		errors.Is(err, ErrRoleNotFound)
}

// IsDuplicateKeyError checks whether the error is a unique constraint violation.
// It handles both PostgreSQL (code 23505) and SQLite ("UNIQUE constraint failed")
// dialects. Shared by user_repo, oauth2_user_repo, memory_repo, and other repos
// that need to detect concurrent inserts on unique indexes.
func IsDuplicateKeyError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	// SQLite: UNIQUE constraint violations produce a distinct message prefix.
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return true
	}
	return false
}

// DuplicateKeyInfo extracts constraint details from a unique constraint violation error.
// Returns empty strings if the error is not a duplicate key error or if details cannot be extracted.
// This is useful for providing more precise error messages that include the constraint name,
// helping to distinguish between different unique constraints on the same table.
func DuplicateKeyInfo(err error) (constraintName, tableName string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, pgErr.TableName
	}
	// SQLite: Parse constraint name from error message if possible.
	// Format: "UNIQUE constraint failed: table.column" or "UNIQUE constraint failed: index_name"
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		// SQLite doesn't provide structured constraint info, return what we have.
		return "", ""
	}
	return "", ""
}

// WrapDuplicateKeyError wraps a duplicate key error with constraint information for better diagnostics.
// If the error is not a duplicate key error, it returns the original error unchanged.
func WrapDuplicateKeyError(err error, context string) error {
	if !IsDuplicateKeyError(err) {
		return err
	}
	constraint, table := DuplicateKeyInfo(err)
	if constraint != "" {
		return fmt.Errorf("%s: duplicate key constraint violation on %q (table: %q): %w", context, constraint, table, err)
	}
	return fmt.Errorf("%s: duplicate key constraint violation: %w", context, err)
}
