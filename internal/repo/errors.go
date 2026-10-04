// Package repo provides data access layer (Repository) implementations.
//
// All repo methods must obtain the database handle via DBFromContext to support
// transparent transaction propagation. Sentinel errors are defined centrally
// in errors.go; callers can use errors.Is to determine error types.
package repo

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Sentinel errors for callers to check via errors.Is.
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
		errors.Is(err, ErrMultipartUploadNotFound)
}

// IsDuplicateKeyError checks whether the error is a PostgreSQL unique
// constraint violation (code 23505).
// Shared by user_repo, oauth2_user_repo, and other repos that need to
// detect concurrent inserts on unique indexes.
func IsDuplicateKeyError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
