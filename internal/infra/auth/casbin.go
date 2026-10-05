// Package auth provides Casbin enforcer integration for RBAC permission checking.
package auth

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	gormadapter "github.com/casbin/gorm-adapter/v3"
	"gorm.io/gorm"
)

// CasbinModelText is the RBAC model definition used by the enforcer.
//
// - r = subject (user ID), object (resource), action
// - p = subject (role ID), object (resource), action
// - g = user-role grouping (g, userID, roleID)
// - Role inheritance via g() matcher function
const CasbinModelText = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && r.obj == p.obj && r.act == p.act
`

// CasbinEnforcer wraps casbin.Enforcer with a mutex for safe concurrent use.
//
// Casbin v2's default Enforcer is NOT goroutine-safe when policies are being
// modified at runtime. We guard all operations with a sync.RWMutex:
//   - Enforce calls use RLock (concurrent reads allowed)
//   - AddPolicy/RemovePolicy use Lock (exclusive write)
type CasbinEnforcer struct {
	enforcer *casbin.Enforcer
	mu       sync.RWMutex
}

// NewCasbinEnforcer creates a Casbin enforcer backed by the given GORM DB.
//
// The gorm-adapter automatically creates the `casbin_rule` table if it does not exist.
// Policies are loaded from the database on construction.
func NewCasbinEnforcer(db *gorm.DB) (*CasbinEnforcer, error) {
	adapter, err := gormadapter.NewAdapterByDBUseTableName(db, "", "casbin_rule")
	if err != nil {
		return nil, fmt.Errorf("create casbin gorm adapter: %w", err)
	}

	m, err := model.NewModelFromString(CasbinModelText)
	if err != nil {
		return nil, fmt.Errorf("create casbin model: %w", err)
	}

	enforcer, err := casbin.NewEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("create casbin enforcer: %w", err)
	}

	// Load all policies from DB into memory
	if err := enforcer.LoadPolicy(); err != nil {
		return nil, fmt.Errorf("load casbin policies: %w", err)
	}

	return &CasbinEnforcer{enforcer: enforcer}, nil
}

// Enforce checks whether a subject (user ID) has permission to perform an action
// on a resource. Returns true if allowed, false otherwise.
func (e *CasbinEnforcer) Enforce(_ context.Context, userID, resource, action string) (bool, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	ok, err := e.enforcer.Enforce(userID, resource, action)
	if err != nil {
		return false, fmt.Errorf("permission check failed: %w", err)
	}
	return ok, nil
}

// AddPolicy adds a permission policy (role -> resource -> action).
// Persists to DB via the adapter.
// Returns (true, nil) if the policy was added, (false, nil) if it already existed.
func (e *CasbinEnforcer) AddPolicy(_ context.Context, roleID, resource, action string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	added, err := e.enforcer.AddPolicy(roleID, resource, action)
	if err != nil {
		return false, fmt.Errorf("add permission policy: %w", err)
	}
	return added, nil
}

// AddPolicies adds multiple permission policies in one batch.
func (e *CasbinEnforcer) AddPolicies(_ context.Context, policies [][]string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.enforcer.AddPolicies(policies); err != nil {
		return fmt.Errorf("add permission policies: %w", err)
	}
	return nil
}

// RemovePolicy removes a permission policy.
func (e *CasbinEnforcer) RemovePolicy(_ context.Context, roleID, resource, action string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.enforcer.RemovePolicy(roleID, resource, action); err != nil {
		return fmt.Errorf("remove permission policy: %w", err)
	}
	return nil
}

// RemovePolicies removes multiple permission policies in one batch.
func (e *CasbinEnforcer) RemovePolicies(_ context.Context, policies [][]string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.enforcer.RemovePolicies(policies); err != nil {
		return fmt.Errorf("remove permission policies: %w", err)
	}
	return nil
}

// AddGroupingPolicy adds a user-role assignment (g, userID, roleID).
func (e *CasbinEnforcer) AddGroupingPolicy(_ context.Context, userID, roleID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.enforcer.AddGroupingPolicy(userID, roleID); err != nil {
		return fmt.Errorf("add user-role assignment: %w", err)
	}
	return nil
}

// RemoveGroupingPolicy removes a user-role assignment.
func (e *CasbinEnforcer) RemoveGroupingPolicy(_ context.Context, userID, roleID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.enforcer.RemoveGroupingPolicy(userID, roleID); err != nil {
		return fmt.Errorf("remove user-role assignment: %w", err)
	}
	return nil
}

// RemoveFilteredGroupingPolicy removes all grouping policies matching the filter.
// fieldIndex is the starting field to filter on (0=subject/userID, 1=object/roleID).
func (e *CasbinEnforcer) RemoveFilteredGroupingPolicy(_ context.Context, fieldIndex int, fieldValues ...string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.enforcer.RemoveFilteredGroupingPolicy(fieldIndex, fieldValues...); err != nil {
		return fmt.Errorf("remove filtered user-role assignments: %w", err)
	}
	return nil
}

// RemoveFilteredPolicy removes all policies matching the given field filter.
// e.g., RemoveFilteredPolicy(ctx, 0, roleID) removes all p policies where v0==roleID.
func (e *CasbinEnforcer) RemoveFilteredPolicy(_ context.Context, fieldIndex int, fieldValues ...string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.enforcer.RemoveFilteredPolicy(fieldIndex, fieldValues...); err != nil {
		return fmt.Errorf("remove filtered permission policies: %w", err)
	}
	return nil
}

// GetPoliciesForRole returns all [resource, action] pairs assigned to a role.
func (e *CasbinEnforcer) GetPoliciesForRole(roleID string) [][]string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	policies, _ := e.enforcer.GetFilteredPolicy(0, roleID)
	result := make([][]string, 0, len(policies))
	for _, p := range policies {
		if len(p) >= 3 {
			result = append(result, []string{p[1], p[2]})
		}
	}
	return result
}

// GetRolesForUser returns all role IDs assigned to a user.
func (e *CasbinEnforcer) GetRolesForUser(userID string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	roles, _ := e.enforcer.GetRolesForUser(userID)
	return roles
}

// GetPermissionsForUser returns all [resource, action] pairs for a user
// (across all roles the user has, following role inheritance).
func (e *CasbinEnforcer) GetPermissionsForUser(userID string) [][]string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	policies, _ := e.enforcer.GetImplicitPermissionsForUser(userID)
	result := make([][]string, 0, len(policies))
	seen := make(map[string]struct{})
	for _, p := range policies {
		if len(p) >= 3 {
			key := p[1] + ":" + p[2]
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				result = append(result, []string{p[1], p[2]})
			}
		}
	}
	return result
}

// HasGroupingPolicy checks if a user-role assignment exists.
func (e *CasbinEnforcer) HasGroupingPolicy(userID, roleID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	ok, err := e.enforcer.HasGroupingPolicy(userID, roleID)
	if err != nil {
		return false
	}
	return ok
}

// GetAllRoles returns all role IDs known to the enforcer (from grouping policies).
func (e *CasbinEnforcer) GetAllRoles() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	roles, _ := e.enforcer.GetAllRoles()
	return roles
}

// Enforcer returns the underlying casbin.Enforcer for advanced usage.
// Use with caution — direct access bypasses the mutex protection.
func (e *CasbinEnforcer) Enforcer() *casbin.Enforcer {
	return e.enforcer
}

// newCasbinEnforcerFromModel creates a CasbinEnforcer from a pre-built model and adapter.
// This is primarily for testing; production code should use NewCasbinEnforcer.
func newCasbinEnforcerFromModel(m model.Model, adapter any) (*CasbinEnforcer, error) {
	enforcer, err := casbin.NewEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("create casbin enforcer: %w", err)
	}
	if err := enforcer.LoadPolicy(); err != nil {
		return nil, fmt.Errorf("load casbin policies: %w", err)
	}
	return &CasbinEnforcer{enforcer: enforcer}, nil
}

// SanitizeModelText returns the model text with extra whitespace normalized.
// Exported for testing.
func SanitizeModelText(s string) string {
	return strings.TrimSpace(s)
}
