// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"github.com/gin-gonic/gin"

	"github.com/rtc-agent/server/internal/infra/auth"
)

// routeResourceMapping maps HTTP method + exact Gin route pattern to (resource, action).
//
// Gin's FullPath() returns the registered pattern (e.g. "/api/roles/:id"), not the
// concrete URL (e.g. "/api/roles/abc-123"). We match against these patterns directly.
type routeResourceMapping struct {
	method   string // HTTP method: "GET", "POST", "PUT", "PATCH", "DELETE"
	pattern  string // Gin route pattern (from FullPath), e.g. "/api/roles/:id"
	resource string // Casbin resource
	action   string // Casbin action
}

// routeResourceMap defines the mapping from API routes to Casbin resources/actions.
//
// Each entry uses the exact Gin route pattern (with :param placeholders).
// This avoids prefix-matching ambiguities where "/api/roles" could match
// both "/api/roles" and "/api/roles/:id/users".
var routeResourceMap = []routeResourceMapping{
	// Role management
	{"GET", "/api/roles", "role", "read"},
	{"POST", "/api/roles", "role", "write"},
	{"PUT", "/api/roles/:id", "role", "write"},
	{"PATCH", "/api/roles/:id", "role", "write"},
	{"DELETE", "/api/roles/:id", "role", "delete"},
	{"GET", "/api/roles/:id", "role", "read"},
	{"GET", "/api/roles/:id/policies", "role", "read"},

	// Permission management
	{"GET", "/api/permissions", "permission", "read"},
	{"POST", "/api/permissions", "permission", "write"},
	{"DELETE", "/api/permissions", "permission", "delete"},
	{"POST", "/api/permissions/check", "permission", "read"},

	// User-role management
	{"GET", "/api/users/:id/roles", "user_role", "read"},
	{"POST", "/api/users/:id/roles", "user_role", "write"},
	{"DELETE", "/api/users/:id/roles/:roleId", "user_role", "delete"},
	{"GET", "/api/roles/:id/users", "user_role", "read"},

	// Audit logs
	{"GET", "/api/audit-logs", "audit_log", "read"},
	{"GET", "/api/audit-logs/:id", "audit_log", "read"},
}

// CasbinMiddleware creates a Gin middleware that checks Casbin permissions.
//
// If the permission system is disabled (permissionSystemEnabled=false), all requests
// are allowed through (legacy behavior). When enabled, requests must have a valid
// Casbin policy for the corresponding (resource, action) pair.
//
// SECURITY POLICY: allow-by-default for unmapped routes.
// Routes not registered in routeResourceMap are accessible to all authenticated users.
// WARNING: New API endpoints MUST be added to routeResourceMap to enforce permissions.
// This design prioritizes developer experience (new endpoints work immediately) over
// strict security. For high-security deployments, consider changing to deny-by-default.
func CasbinMiddleware(enforcer *auth.CasbinEnforcer, permissionSystemEnabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// If permission system is disabled, allow all requests (legacy mode)
		if !permissionSystemEnabled {
			c.Next()
			return
		}

		// SECURITY: Reject requests without a valid user_id (P0 #3 fix).
		// The JWT middleware sets user_id in the context. If it's missing or empty,
		// the request was not properly authenticated — deny access.
		userIDStr, exists := c.Get("user_id")
		if !exists {
			Error(c, "unauthorized", "未认证")
			c.Abort()
			return
		}

		userID, ok := userIDStr.(string)
		if !ok || userID == "" {
			Error(c, "unauthorized", "未认证")
			c.Abort()
			return
		}

		// Find matching route mapping using exact method + pattern match (P0 #4 fix)
		method := c.Request.Method
		pattern := c.FullPath()
		if pattern == "" {
			// FullPath returns empty for unregistered routes — let Gin handle 404
			c.Next()
			return
		}

		resource, action, found := lookupRouteMapping(method, pattern)
		if !found {
			// SECURITY: allow-by-default for unmapped routes (per design doc section 3.4).
			// WARNING: This means any registered route not in routeResourceMap is accessible
			// to all authenticated users. New API endpoints MUST be added to routeResourceMap
			// to enforce proper permission checks. This is a deliberate design trade-off
			// favoring developer convenience over strict security.
			c.Next()
			return
		}

		// Check permission
		allowed, err := enforcer.Enforce(c.Request.Context(), userID, resource, action)
		if err != nil {
			Error(c, "server_error", "权限检查失败")
			c.Abort()
			return
		}

		if !allowed {
			Error(c, "forbidden", "权限不足")
			c.Abort()
			return
		}

		c.Next()
	}
}

// lookupRouteMapping finds the resource and action for a given method + pattern.
// Uses exact match on both method and pattern to avoid prefix-matching conflicts.
func lookupRouteMapping(method, pattern string) (resource, action string, found bool) {
	for _, m := range routeResourceMap {
		if m.method == method && m.pattern == pattern {
			return m.resource, m.action, true
		}
	}
	return "", "", false
}
