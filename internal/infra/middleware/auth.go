// Package middleware provides HTTP middleware implementations.
package middleware

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/pkg/logger"
)

// UserBanChecker checks if a user account is banned.
// Implemented by repo.OAuth2UserRepo to allow middleware to check ban status
// without handler importing repo package directly.
type UserBanChecker interface {
	IsUserBanned(ctx context.Context, userID uuid.UUID) (bool, error)
}

// banKeyPrefix is the Redis key prefix for ban status cache.
// Format: rtc:ban:status:{userID}
// The "rtc:" prefix ensures consistency with other project Redis keys and prevents
// conflicts in shared Redis environments.
const banKeyPrefix = "rtc:ban:status:"

// BanCache provides a Redis-backed shared cache for ban status checks.
// All server instances share the same cache, ensuring consistency in distributed deployments.
type BanCache struct {
	rdb redis.UniversalClient
	ttl time.Duration
}

// NewBanCache creates a new Redis-backed ban cache with the specified TTL.
func NewBanCache(rdb redis.UniversalClient, ttl time.Duration) *BanCache {
	return &BanCache{
		rdb: rdb,
		ttl: ttl,
	}
}

// banCacheKey returns the Redis key for a user's ban status.
func banCacheKey(userID uuid.UUID) string {
	return banKeyPrefix + userID.String()
}

// Get retrieves a cached ban status if it exists and hasn't expired.
// Returns (banned, cached) where cached indicates if the value was found in cache.
func (c *BanCache) Get(ctx context.Context, userID uuid.UUID) (bool, bool) {
	if c.rdb == nil {
		return false, false
	}

	key := banCacheKey(userID)
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			return false, false
		}
		logger.Warn(ctx, "ban_cache.get_failed",
			zap.String("user_id", userID.String()),
			zap.Error(err))
		return false, false
	}

	// Value is "1" for banned, "0" for not banned
	banned := val == "1"
	return banned, true
}

// Set stores a ban status in the cache with TTL.
func (c *BanCache) Set(ctx context.Context, userID uuid.UUID, banned bool) {
	if c.rdb == nil {
		return
	}

	key := banCacheKey(userID)
	val := "0"
	if banned {
		val = "1"
	}

	if err := c.rdb.Set(ctx, key, val, c.ttl).Err(); err != nil {
		logger.Warn(ctx, "ban_cache.set_failed",
			zap.String("user_id", userID.String()),
			zap.Bool("banned", banned),
			zap.Error(err))
	}
}

// Invalidate removes a user's ban status from the cache.
func (c *BanCache) Invalidate(ctx context.Context, userID uuid.UUID) {
	if c.rdb == nil {
		return
	}

	key := banCacheKey(userID)
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		logger.Warn(ctx, "ban_cache.invalidate_failed",
			zap.String("user_id", userID.String()),
			zap.Error(err))
	}
}

// globalBanCache is a shared cache instance for ban status checks across all JWT middleware instances.
// Initialized via InitBanCache during server startup. Uses atomic.Pointer for lock-free, thread-safe
// reads after initialization (P1 data race fix).
var globalBanCache atomic.Pointer[BanCache]

// InitBanCache initializes the global ban cache with a Redis client.
// Must be called during server startup before any requests are processed.
// Thread-safe: uses atomic.Pointer.Store for safe concurrent publication.
func InitBanCache(rdb redis.UniversalClient, ttl time.Duration) {
	cache := NewBanCache(rdb, ttl)
	globalBanCache.Store(cache)
}

// InvalidateBanCache removes a user's ban status from the global cache.
// Called when a user is unbanned to ensure immediate effect.
func InvalidateBanCache(ctx context.Context, userID uuid.UUID) {
	if cache := globalBanCache.Load(); cache != nil {
		cache.Invalidate(ctx, userID)
	}
}

// GetBanCache retrieves a user's ban status from the global cache.
// Returns (banned, cached) where cached indicates if the value was found in cache.
func GetBanCache(ctx context.Context, userID uuid.UUID) (bool, bool) {
	cache := globalBanCache.Load()
	if cache == nil {
		return false, false
	}
	return cache.Get(ctx, userID)
}

// SetBanCache stores a user's ban status in the global cache.
func SetBanCache(ctx context.Context, userID uuid.UUID, banned bool) {
	if cache := globalBanCache.Load(); cache != nil {
		cache.Set(ctx, userID, banned)
	}
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

			// 3. Check if user is banned (with Redis cache)
			// SECURITY NOTE: This implements a fail-open strategy for resilience:
			// - If Redis is unavailable, Get returns (false, false) → falls through to DB check
			// - If DB is also unavailable, the request is allowed to proceed (fail-open)
			// This ensures system availability over strict security in extreme failure scenarios.
			// The 30-second cache TTL limits the window where a banned user might slip through.
			if banChecker != nil {
				banned, cached := GetBanCache(r.Context(), userID)
				if !cached {
					var err error
					banned, err = banChecker.IsUserBanned(r.Context(), userID)
					if err != nil {
						logger.Warn(r.Context(), "failed to check user ban status",
							zap.Error(err),
							zap.String("user_id", userID.String()))
						// Fail-open: allow request to proceed when both cache and DB are unavailable
						// This is a deliberate trade-off: availability > strict security
					} else {
						SetBanCache(r.Context(), userID, banned)
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
