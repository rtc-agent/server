// Package usecase/primitives - errors.go
// Error handling utilities for the usecase layer.
package primitives

import "github.com/rtc-agent/server/internal/repo"

// IsNotFound checks whether the error is a "not found" variant.
// This is a wrapper around repo.IsNotFound to allow Handler to check
// errors without importing repo directly.
func IsNotFound(err error) bool {
	return repo.IsNotFound(err)
}

// Sentinel errors re-exported from repo package.
// These allow Handler to check specific error conditions without importing repo.
var (
	ErrSessionNotFound         = repo.ErrSessionNotFound
	ErrSessionClosed           = repo.ErrSessionClosed
	ErrSessionClosedOrNotFound = repo.ErrSessionClosedOrNotFound
	ErrPermissionDenied        = repo.ErrPermissionDenied
)

// SessionRepo is the interface for session data access.
// Re-exported from repo package to allow Handler to declare dependencies
// without importing repo directly.
type SessionRepo = repo.SessionRepo

// ScriptExecutionRepo is the interface for script execution data access.
// Re-exported from repo package to allow Handler to declare dependencies
// without importing repo directly.
type ScriptExecutionRepo = repo.ScriptExecutionRepo

// FileRepo is the interface for file data access.
// Re-exported from repo package to allow Handler to declare dependencies
// without importing repo directly.
type FileRepo = repo.FileRepo
