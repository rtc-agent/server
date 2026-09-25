package websearch

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua scripts for atomic circuit breaker operations

// allowLua checks if a request is allowed and handles state transitions
// KEYS[1]: websearch:cb:{provider}
// ARGV[1]: open_timeout (seconds)
// ARGV[2]: current timestamp (unix seconds)
// ARGV[3]: half_open_max_requests
// Returns: 1 = allow, 0 = reject
// Note: Redis Lua scripts execute atomically -- no other command can interleave,
// so a single HGET of state is sufficient (no redundant CAS needed).
const allowLua = `
local key = KEYS[1]
local open_timeout = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local max_half_open = tonumber(ARGV[3])

local state = redis.call('HGET', key, 'state') or 'closed'
local last_failure = tonumber(redis.call('HGET', key, 'last_failure') or '0')
local half_open_count = tonumber(redis.call('HGET', key, 'half_open_count') or '0')

if state == 'closed' then
    return 1
elseif state == 'open' then
    if (now - last_failure) >= open_timeout then
        -- Lua is atomic: state cannot change mid-script, so no CAS needed
        redis.call('HSET', key, 'state', 'half_open', 'half_open_count', '1', 'half_open_success_count', '0')
        -- Set TTL on both main key and window list to prevent orphaned keys
        local ttl = open_timeout * 2
        redis.call('EXPIRE', key, ttl)
        if redis.call('EXISTS', key .. ':window') == 1 then
            redis.call('EXPIRE', key .. ':window', ttl)
        end
        return 1
    end
    return 0
elseif state == 'half_open' then
    if half_open_count < max_half_open then
        redis.call('HINCRBY', key, 'half_open_count', 1)
        return 1
    end
    return 0
end
return 0
`

// recordLua records an outcome and handles state transitions
// KEYS[1]: websearch:cb:{provider}
// ARGV[1]: success ("1" or "0")
// ARGV[2]: current timestamp
// ARGV[3]: failure_threshold (0-100)
// ARGV[4]: window_size
// ARGV[5]: half_open_max_requests
// ARGV[6]: open_timeout (seconds), used for window list TTL
// Returns: new state ("closed", "open", "half_open")
const recordLua = `
local key = KEYS[1]
local success = ARGV[1] == "1"
local now = tonumber(ARGV[2])
local threshold = tonumber(ARGV[3])
local window_size = tonumber(ARGV[4])
local half_open_max = tonumber(ARGV[5])
local open_timeout = tonumber(ARGV[6])
local window_key = key .. ':window'

-- Check state first; skip window updates when circuit is open (no requests allowed)
local state = redis.call('HGET', key, 'state') or 'closed'
if state == 'open' then
    -- Still update last_failure so the open timeout countdown restarts
    if not success then
        redis.call('HSET', key, 'last_failure', tostring(now))
        -- Reset TTL to prevent key expiration while timeout keeps resetting
        local ttl = open_timeout * 2
        redis.call('EXPIRE', key, ttl)
    end
    return 'open'
end

-- Use list to store recent window_size records ("1"=success, "0"=failure)
local record = success and "1" or "0"
redis.call('LPUSH', window_key, record)
redis.call('LTRIM', window_key, 0, window_size - 1)
-- Keep window list TTL in sync with main key to prevent orphaned keys
redis.call('EXPIRE', window_key, open_timeout * 2)

-- Ensure main key exists in closed state to prevent orphaned keys
-- when circuit never opens (e.g., continuous successes)
if state == 'closed' then
    -- Create key if it doesn't exist (first success in closed state)
    if redis.call('EXISTS', key) == 0 then
        redis.call('HSET', key, 'state', 'closed')
    end
    redis.call('EXPIRE', key, open_timeout * 2)
end

if not success then
    redis.call('HSET', key, 'last_failure', tostring(now))
end

-- Calculate failure rate
local window = redis.call('LRANGE', window_key, 0, -1)
local failures = 0
for _, v in ipairs(window) do
    if v == "0" then failures = failures + 1 end
end
local failure_rate = (#window > 0) and (failures * 100 / #window) or 0

if state == 'half_open' then
    if success then
        -- Increment success count
        local success_count = tonumber(redis.call('HINCRBY', key, 'half_open_success_count', 1))

        -- Only restore to closed when reaching required successes
        if success_count >= half_open_max then
            redis.call('HSET', key, 'state', 'closed', 'half_open_count', '0', 'half_open_success_count', '0')
            redis.call('DEL', window_key)  -- Reset window
            return 'closed'
        end
        return 'half_open'
    else
        -- Reset both counters and clear stale window when failing in half_open
        redis.call('HSET', key, 'state', 'open', 'half_open_count', '0', 'half_open_success_count', '0')
        redis.call('DEL', window_key)  -- Clear stale window data
        -- Set TTL to prevent orphaned keys if no requests come in
        local ttl = open_timeout * 2
        redis.call('EXPIRE', key, ttl)
        return 'open'
    end
elseif state == 'closed' then
    -- Only check threshold when window is full to avoid premature opening.
    -- When threshold=0 (any failure opens), require at least one actual failure
    -- so that pure-success windows do not accidentally trip the breaker.
    if #window >= window_size and failures > 0 and (threshold == 0 or failure_rate >= threshold) then
        redis.call('HSET', key, 'state', 'open')
        -- Set TTL to prevent orphaned keys if no requests come in
        local ttl = open_timeout * 2
        redis.call('EXPIRE', key, ttl)
        return 'open'
    end
end
return state
`

var (
	allowScript  = redis.NewScript(allowLua)
	recordScript = redis.NewScript(recordLua)
)

// DistributedCircuitBreaker implements circuit breaker using Redis
type DistributedCircuitBreaker struct {
	providerName string
	redisClient  *redis.Client
	config       CircuitBreakerConfig
}

// NewDistributedCircuitBreaker creates a distributed circuit breaker
func NewDistributedCircuitBreaker(
	providerName string,
	redisClient *redis.Client,
	config CircuitBreakerConfig,
) *DistributedCircuitBreaker {
	// Validate configuration to prevent subtle bugs
	if config.WindowSize <= 0 {
		config.WindowSize = 10 // Default window size
	}
	if config.FailureThreshold < 0 || config.FailureThreshold > 100 {
		config.FailureThreshold = 50 // Default 50%
	}
	if config.OpenTimeout < time.Second {
		config.OpenTimeout = 5 * time.Second // Default 5s, ensure >= 1s for second-precision
	}
	if config.HalfOpenMaxRequests <= 0 {
		config.HalfOpenMaxRequests = 3 // Default 3 probe requests
	}

	return &DistributedCircuitBreaker{
		providerName: providerName,
		redisClient:  redisClient,
		config:       config,
	}
}

// AllowWithErr checks if a request is allowed (returns error for Redis failures)
func (d *DistributedCircuitBreaker) AllowWithErr(ctx context.Context) (bool, error) {
	key := fmt.Sprintf("websearch:cb:%s", d.providerName)
	result, err := allowScript.Run(
		ctx, d.redisClient,
		[]string{key},
		int(d.config.OpenTimeout.Seconds()),
		time.Now().Unix(),
		d.config.HalfOpenMaxRequests,
	).Int()
	if err != nil {
		return true, err // Return error for caller to handle degradation
	}
	return result == 1, nil
}

// RecordOutcome records a success or failure outcome
func (d *DistributedCircuitBreaker) RecordOutcome(ctx context.Context, success bool) error {
	key := fmt.Sprintf("websearch:cb:%s", d.providerName)
	successArg := "0"
	if success {
		successArg = "1"
	}

	_, err := recordScript.Run(
		ctx, d.redisClient,
		[]string{key},
		successArg,
		time.Now().Unix(),
		d.config.FailureThreshold,
		d.config.WindowSize,
		d.config.HalfOpenMaxRequests,
		int(d.config.OpenTimeout.Seconds()), // ARGV[6]: open_timeout for window TTL
	).Result()
	if err != nil {
		return err
	}
	return nil
}
