// Package usecase provides business logic implementations.
//
// role_errors.go exports sentinel errors from the repo layer for use by
// the handler layer. Handlers cannot import repo directly (depguard),
// so these aliases provide the same errors through the usecase package.
package usecase

import "github.com/rtc-agent/server/internal/repo"

// Role and permission system error aliases.
// These are the same underlying errors as in repo, re-exported so that
// handlers can use errors.Is without importing repo.
var (
	ErrNotFound               = repo.ErrNotFound
	ErrRoleNotFound           = repo.ErrRoleNotFound
	ErrRoleNameExists         = repo.ErrRoleNameExists
	ErrRoleDisabled           = repo.ErrRoleDisabled
	ErrCannotDeleteSystemRole = repo.ErrCannotDeleteSystemRole
	ErrCannotRemoveLastAdmin  = repo.ErrCannotRemoveLastAdmin
	ErrCannotRemoveSelfAdmin  = repo.ErrCannotRemoveSelfAdmin
	ErrConflict               = repo.ErrConflict
	ErrPermissionExists       = repo.ErrPermissionExists
)

// IsNotFound checks whether the error is a "not found" variant.
func IsNotFound(err error) bool {
	return repo.IsNotFound(err)
}
