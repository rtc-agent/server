package auth

import (
	"context"
	"testing"

	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockAdapter is a simple in-memory adapter for testing.
type mockAdapter struct {
	policies [][]string
}

func newMockAdapter() *mockAdapter {
	return &mockAdapter{policies: make([][]string, 0)}
}

func (a *mockAdapter) LoadPolicy(model model.Model) error {
	for _, p := range a.policies {
		if len(p) < 3 {
			continue
		}
		ptype := p[0]
		sec := ptype[:1]
		key := sec
		rule := p[1:]
		model.AddPolicy(sec, key, rule)
	}
	return nil
}

func (a *mockAdapter) SavePolicy(model model.Model) error {
	return nil
}

func (a *mockAdapter) AddPolicy(sec string, ptype string, rule []string) error {
	return nil
}

func (a *mockAdapter) RemovePolicy(sec string, ptype string, rule []string) error {
	return nil
}

func (a *mockAdapter) RemoveFilteredPolicy(sec string, ptype string, fieldIndex int, fieldValues ...string) error {
	return nil
}

// AddPolicies adds multiple rules to the policy (implements persist.BatchAdapter).
func (a *mockAdapter) AddPolicies(sec string, ptype string, rules [][]string) error {
	return nil
}

// RemovePolicies removes multiple rules from the policy (implements persist.BatchAdapter).
func (a *mockAdapter) RemovePolicies(sec string, ptype string, rules [][]string) error {
	return nil
}

var _ persist.Adapter = (*mockAdapter)(nil)
var _ persist.BatchAdapter = (*mockAdapter)(nil)

func newTestEnforcer(t *testing.T) *CasbinEnforcer {
	t.Helper()
	m, err := model.NewModelFromString(CasbinModelText)
	require.NoError(t, err)

	adapter := newMockAdapter()
	enforcer, err := newCasbinEnforcerFromModel(m, adapter)
	require.NoError(t, err)
	return enforcer
}

// addTestPolicy is a test helper that adds a policy and asserts no error.
// AddPolicy returns (bool, error); this helper discards the bool for test convenience.
func addTestPolicy(t *testing.T, e *CasbinEnforcer, ctx context.Context, roleID, resource, action string) {
	t.Helper()
	_, err := e.AddPolicy(ctx, roleID, resource, action)
	require.NoError(t, err)
}

// TestEnforce_BasicRBAC tests basic RBAC permission checking.
func TestEnforce_BasicRBAC(t *testing.T) {
	e := newTestEnforcer(t)
	ctx := context.Background()

	// Add role policy: role-1 can read user
	addTestPolicy(t, e, ctx, "role-1", "user", "read")
	// Add grouping: user-1 has role-1
	require.NoError(t, e.AddGroupingPolicy(ctx, "user-1", "role-1"))

	// user-1 should be able to read user
	ok, err := e.Enforce(ctx, "user-1", "user", "read")
	require.NoError(t, err)
	assert.True(t, ok)

	// user-1 should NOT be able to write user
	ok, err = e.Enforce(ctx, "user-1", "user", "write")
	require.NoError(t, err)
	assert.False(t, ok)

	// user-2 should NOT be able to read user (no role assigned)
	ok, err = e.Enforce(ctx, "user-2", "user", "read")
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestAddAndRemovePolicy tests adding and removing policies.
func TestAddAndRemovePolicy(t *testing.T) {
	e := newTestEnforcer(t)
	ctx := context.Background()

	addTestPolicy(t, e, ctx, "role-1", "user", "read")
	addTestPolicy(t, e, ctx, "role-1", "user", "write")

	policies := e.GetPoliciesForRole("role-1")
	assert.Len(t, policies, 2)

	require.NoError(t, e.RemovePolicy(ctx, "role-1", "user", "write"))

	policies = e.GetPoliciesForRole("role-1")
	assert.Len(t, policies, 1)
	assert.Equal(t, []string{"user", "read"}, policies[0])
}

// TestGroupingPolicy tests user-role assignment operations.
func TestGroupingPolicy(t *testing.T) {
	e := newTestEnforcer(t)
	ctx := context.Background()

	require.NoError(t, e.AddGroupingPolicy(ctx, "user-1", "role-1"))
	require.NoError(t, e.AddGroupingPolicy(ctx, "user-1", "role-2"))

	roles := e.GetRolesForUser("user-1")
	assert.Len(t, roles, 2)

	assert.True(t, e.HasGroupingPolicy("user-1", "role-1"))
	assert.False(t, e.HasGroupingPolicy("user-1", "role-3"))

	require.NoError(t, e.RemoveGroupingPolicy(ctx, "user-1", "role-1"))
	assert.False(t, e.HasGroupingPolicy("user-1", "role-1"))
}

// TestRemoveFilteredPolicy tests batch removal of policies.
func TestRemoveFilteredPolicy(t *testing.T) {
	e := newTestEnforcer(t)
	ctx := context.Background()

	addTestPolicy(t, e, ctx, "role-1", "user", "read")
	addTestPolicy(t, e, ctx, "role-1", "user", "write")
	addTestPolicy(t, e, ctx, "role-1", "role", "read")

	require.NoError(t, e.RemoveFilteredPolicy(ctx, 0, "role-1"))

	policies := e.GetPoliciesForRole("role-1")
	assert.Len(t, policies, 0)
}

// TestBatchPolicies tests batch add/remove operations.
func TestBatchPolicies(t *testing.T) {
	e := newTestEnforcer(t)
	ctx := context.Background()

	policies := [][]string{
		{"role-1", "user", "read"},
		{"role-1", "user", "write"},
		{"role-1", "role", "read"},
	}
	require.NoError(t, e.AddPolicies(ctx, policies))

	result := e.GetPoliciesForRole("role-1")
	assert.Len(t, result, 3)

	require.NoError(t, e.RemovePolicies(ctx, policies[:2]))

	result = e.GetPoliciesForRole("role-1")
	assert.Len(t, result, 1)
}

// TestGetPermissionsForUser tests aggregated permissions for a user.
func TestGetPermissionsForUser(t *testing.T) {
	e := newTestEnforcer(t)
	ctx := context.Background()

	// Two roles with overlapping permissions
	addTestPolicy(t, e, ctx, "role-1", "user", "read")
	addTestPolicy(t, e, ctx, "role-2", "user", "read") // duplicate
	addTestPolicy(t, e, ctx, "role-2", "role", "read")

	require.NoError(t, e.AddGroupingPolicy(ctx, "user-1", "role-1"))
	require.NoError(t, e.AddGroupingPolicy(ctx, "user-1", "role-2"))

	perms := e.GetPermissionsForUser("user-1")
	// Should have 2 unique permissions (user:read, role:read), not 3
	assert.Len(t, perms, 2)
}
