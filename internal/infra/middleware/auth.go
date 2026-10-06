// Package middleware provides HTTP middleware implementations.
package middleware

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/pkg/logger"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// UserBanChecker checks if a user account is banned.
// Implemented by repo.OAuth2UserRepo to allow middleware to check ban status
// without handler importing repo package directly.
type UserBanChecker interface {
	IsUserBanned(ctx context.Context, userID uuid.UUID) (bool, error)
}

// banCacheEntry holds a cached ban check result with expiration.
type banCacheEntry struct {
	banned    bool
	expiresAt time.Time
}

// banCache provides a simple TTL-based cache for ban status checks.
type banCache struct {
	mu      sync.RWMutex
	entries map[uuid.UUID]banCacheEntry
	ttl     time.Duration
}

// newBanCache creates a new ban cache with the specified TTL.
// Starts a background goroutine to periodically clean up expired entries.
func newBanCache(ttl time.Duration) *banCache {
	c := &banCache{
		entries: make(map[uuid.UUID]banCacheEntry),
		ttl:     ttl,
	}
	// Start cleanup goroutine
	go c.cleanupLoop()
	return c
}

// cleanupLoop periodically removes expired entries to prevent memory leaks.
func (c *banCache) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		c.cleanup()
	}
}

// cleanup removes all expired entries from the cache.
func (c *banCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for userID, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, userID)
		}
	}
}

// get retrieves a cached ban status if it exists and hasn't expired.
func (c *banCache) get(userID uuid.UUID) (bool, bool) {
	c.mu.RLock()
	entry, ok := c.entries[userID]
	c.mu.RUnlock()

	if !ok || time.Now().After(entry.expiresAt) {
		return false, false
	}
	return entry.banned, true
}

// set stores a ban status in the cache.
func (c *banCache) set(userID uuid.UUID, banned bool) {
	c.mu.Lock()
	c.entries[userID] = banCacheEntry{
		banned:    banned,
		expiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// invalidate removes a user's ban status from the cache.
func (c *banCache) invalidate(userID uuid.UUID) {
	c.mu.Lock()
	delete(c.entries, userID)
	c.mu.Unlock()
}

// globalBanCache is a shared cache instance for ban status checks across all JWT middleware instances.
var globalBanCache = newBanCache(30 * time.Second)

// InvalidateBanCache removes a user's ban status from the global cache.
// Called when a user is unbanned to ensure immediate effect.
func InvalidateBanCache(userID uuid.UUID) {
	globalBanCache.invalidate(userID)
}

// GetBanCache retrieves a user's ban status from the global cache.
// Returns (banned, cached) where cached indicates if the value was found in cache.
func GetBanCache(userID uuid.UUID) (bool, bool) {
	return globalBanCache.get(userID)
}

// SetBanCache stores a user's ban status in the global cache.
func SetBanCache(userID uuid.UUID, banned bool) {
	globalBanCache.set(userID, banned)
}

// JWTAuth creates a JWT authentication middleware.
// signer is the JWT signer; allowDevBypass should only be true in development,
// allowing X-User-ID / X-Device-ID headers to bypass JWT validation (must be false in production).
// banChecker optionally checks if user is banned (can be nil to skip check).
func JWTAuth(signer *auth.JWTSigner, allowDevBypass bool, banChecker UserBanChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var userID uuid.UUID
			var deviceID string

			// 1. Try to parse JWT from Authorization header.
			if token := extractBearerToken(r); token != "" {
				claims, err := signer.ParseAccessToken(token)
				if err != nil {
					logger.Warn(r.Context(), "JWT validation failed",
						zap.Error(err),
						zap.String("remote_addr", r.RemoteAddr))
					http.Error(w, "unauthorized: invalid token", http.StatusUnauthorized)
					return
				}
				userID = claims.UserID
				deviceID = claims.DeviceID
			} else if allowDevBypass {
				// 2. Dev fallback: read directly from headers (must be explicitly enabled).
				// Log a warning every time dev bypass is used for audit trail.
				logger.Warn(r.Context(), "DEV BYPASS: JWT authentication skipped",
					zap.String("remote_addr", r.RemoteAddr),
					zap.String("user_id_header", r.Header.Get("X-User-ID")))
				uidStr := r.Header.Get("X-User-ID")
				did := r.Header.Get("X-Device-ID")
				if uidStr == "" || did == "" {
					auth.RecordFailure("http", "missing")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				parsed, err := uuid.Parse(uidStr)
				if err != nil {
					auth.RecordFailure("http", "invalid")
					http.Error(w, "unauthorized: invalid user id", http.StatusUnauthorized)
					return
				}
				userID = parsed
				deviceID = did
			} else {
				auth.RecordFailure("http", "missing")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			// 3. Check if user is banned (with cache)
			if banChecker != nil {
				banned, cached := globalBanCache.get(userID)
				if !cached {
					var err error
					banned, err = banChecker.IsUserBanned(r.Context(), userID)
					if err != nil {
						logger.Warn(r.Context(), "failed to check user ban status",
							zap.Error(err),
							zap.String("user_id", userID.String()))
						// Continue - don't block legitimate users on DB errors
					} else {
						globalBanCache.set(userID, banned)
					}
				}

				if banned {
					logger.Info(r.Context(), "banned user rejected",
						zap.String("user_id", userID.String()),
						zap.String("remote_addr", r.RemoteAddr))
					http.Error(w, "forbidden: account has been banned", http.StatusForbidden)
					return
				}
			}

			ctx := r.Context()
			ctx = contextx.WithClientInfo(ctx, userID, deviceID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearerToken extracts the token from the Authorization header.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}
