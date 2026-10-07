// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"fmt"
	"log"
	"net/http"

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

// routeMapKey builds the composite key for routeMap lookups: "{METHOD} {pattern}".
func routeMapKey(method, pattern string) string {
	return method + " " + pattern
}

// routeMap provides O(1) lookup from "{method} {pattern}" to its Casbin (resource, action).
// Built once in init() from routeResourceMap — zero per-request allocation.
var routeMap map[string]struct {
	resource string
	action   string
}

func init() {
	routeMap = make(map[string]struct {
		resource string
		action   string
	}, len(routeResourceMap))

	for _, m := range routeResourceMap {
		key := routeMapKey(m.method, m.pattern)
		if existing, dup := routeMap[key]; dup {
			// Fail fast at startup: duplicate route mappings indicate a configuration
			// error that would silently cause incorrect permission checks.
			log.Fatalf("FATAL: duplicate route mapping in routeResourceMap: key=%q, "+
				"existing=(%s, %s), duplicate=(%s, %s)",
				key, existing.resource, existing.action, m.resource, m.action)
		}
		routeMap[key] = struct {
			resource string
			action   string
		}{m.resource, m.action}
	}
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

	// Admin user-role management
	{"GET", "/api/admin-users", "admin_user", "read"},
	{"POST", "/api/admin-users", "admin_user", "write"},
	{"PUT", "/api/admin-users/:id", "admin_user", "write"},
	{"GET", "/api/admin-users/:id/roles", "admin_user_role", "read"},
	{"POST", "/api/admin-users/:id/roles", "admin_user_role", "write"},
	{"DELETE", "/api/admin-users/:id/roles/:roleId", "admin_user_role", "delete"},
	{"GET", "/api/roles/:id/admin-users", "admin_user_role", "read"},

	// Audit logs
	{"GET", "/api/audit-logs", "audit_log", "read"},
	{"GET", "/api/audit-logs/:id", "audit_log", "read"},

	// RTC user management
	{"GET", "/api/rtc-users", "rtc_user", "read"},
	{"GET", "/api/rtc-users/:id", "rtc_user", "read"},
	{"GET", "/api/rtc-users/:id/devices", "rtc_user", "read"},
	{"POST", "/api/rtc-users/:id/ban", "rtc_user", "ban"},
	{"POST", "/api/rtc-users/:id/unban", "rtc_user", "ban"},

	// Session management
	{"GET", "/api/rtc-users/sessions", "rtc_session", "read"},
	{"GET", "/api/rtc-users/sessions/stats", "rtc_session", "read"},
	{"GET", "/api/rtc-users/sessions/:id/messages", "rtc_message", "read"},

	// System-level dynamic configuration management
	{"GET", "/api/configs", "server_config", "read"},
	{"GET", "/api/configs/:key", "server_config", "read"},
	{"PUT", "/api/configs/:key", "server_config", "write"},
	{"DELETE", "/api/configs/:key", "server_config", "delete"},
	{"GET", "/api/configs/:key/history", "server_config", "read"},
	{"POST", "/api/configs/:key/rollback", "server_config", "write"},

	// User-level config overrides (reuse server_config resource)
	{"GET", "/api/rtc-users/:id/configs", "server_config", "read"},
	{"GET", "/api/rtc-users/:id/configs/:key", "server_config", "read"},
	{"PUT", "/api/rtc-users/:id/configs/:key", "server_config", "write"},
	{"DELETE", "/api/rtc-users/:id/configs/:key", "server_config", "delete"},
	{"GET", "/api/rtc-users/:id/configs/:key/history", "server_config", "read"},
	{"POST", "/api/rtc-users/:id/configs/:key/rollback", "server_config", "write"},

	// Dashboard metrics proxy (Prometheus reverse proxy)
	{"GET", "/api/metrics/*path", "dashboard", "read"},
	{"POST", "/api/metrics/*path", "dashboard", "read"},
	{"PUT", "/api/metrics/*path", "dashboard", "read"},
	{"DELETE", "/api/metrics/*path", "dashboard", "read"},
	{"PATCH", "/api/metrics/*path", "dashboard", "read"},

	// Grafana dashboard iframe proxy
	{"GET", "/api/grafana/*path", "dashboard", "read"},
	{"POST", "/api/grafana/*path", "dashboard", "read"},
	{"PUT", "/api/grafana/*path", "dashboard", "read"},
	{"DELETE", "/api/grafana/*path", "dashboard", "read"},
	{"PATCH", "/api/grafana/*path", "dashboard", "read"},

	// Jaeger tracing proxy
	{"GET", "/api/jaeger/*path", "dashboard", "read"},
	{"POST", "/api/jaeger/*path", "dashboard", "read"},
	{"PUT", "/api/jaeger/*path", "dashboard", "read"},
	{"DELETE", "/api/jaeger/*path", "dashboard", "read"},
	{"PATCH", "/api/jaeger/*path", "dashboard", "read"},

	// Pyroscope profiling proxy
	{"GET", "/api/pyroscope/*path", "dashboard", "read"},
	{"POST", "/api/pyroscope/*path", "dashboard", "read"},
	{"PUT", "/api/pyroscope/*path", "dashboard", "read"},
	{"DELETE", "/api/pyroscope/*path", "dashboard", "read"},
	{"PATCH", "/api/pyroscope/*path", "dashboard", "read"},
}

// CasbinMiddleware creates a Gin middleware that checks Casbin permissions.
//
// If the permission system is disabled (permissionSystemEnabled=false), all requests
// are allowed through (legacy behavior). When enabled, requests must have a valid
// Casbin policy for the corresponding (resource, action) pair.
//
// SECURITY POLICY: deny-by-default for unmapped routes.
// Routes not registered in routeResourceMap are REJECTED with 403 Forbidden.
// New API endpoints MUST be added to routeResourceMap, otherwise they will be
// inaccessible to all users (including admins) until the mapping is added.
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
			Error(c, "unauthorized", "Authentication required")
			c.Abort()
			return
		}

		userID, ok := userIDStr.(string)
		if !ok || userID == "" {
			Error(c, "unauthorized", "Authentication required")
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

		// SECURITY: Self-service profile update bypasses Casbin.
		// PUT /api/admin-users/me only modifies the caller's own data (userID from JWT,
		// not request body). This is safe for all authenticated users — no privilege
		// escalation possible. Bypassing Casbin avoids unnecessary policy configuration.
		if method == "PUT" && pattern == "/api/admin-users/me" {
			c.Next()
			return
		}

		resource, action, found := lookupRouteMapping(method, pattern)
		if !found {
			// SECURITY: deny-by-default for unmapped routes (HTTP 403).
			// Any route that is not explicitly mapped in routeResourceMap is forbidden.
			// Developers: if you add a new API endpoint, you MUST add a corresponding
			// entry to routeResourceMap, otherwise the endpoint will be inaccessible.
			// Note: Uses HTTP 403 (not project's usual HTTP 200) because this is a
			// security policy violation — unmapped endpoints should never be reached.
			c.AbortWithStatusJSON(http.StatusForbidden, ResponseStructure{
				Success:      false,
				ErrorCode:    "forbidden",
				ErrorMessage: fmt.Sprintf("permission not configured for this endpoint: %s %s", method, pattern),
			})
			return
		}

		// Check permission
		allowed, err := enforcer.Enforce(c.Request.Context(), userID, resource, action)
		if err != nil {
			Error(c, "server_error", "permission check failed")
			c.Abort()
			return
		}

		if !allowed {
			Error(c, "forbidden", "permission denied")
			c.Abort()
			return
		}

		c.Next()
	}
}

// lookupRouteMapping finds the resource and action for a given method + pattern.
// Uses exact match on both method and pattern to avoid prefix-matching conflicts.
// Lookup is O(1) via the pre-built routeMap index (initialized in init()).
func lookupRouteMapping(method, pattern string) (resource, action string, found bool) {
	key := routeMapKey(method, pattern)
	mapping, ok := routeMap[key]
	if !ok {
		return "", "", false
	}
	return mapping.resource, mapping.action, true
}
