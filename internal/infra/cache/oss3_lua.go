package cache

import "github.com/redis/go-redis/v9"

// OSS3 Lua script names, registered at startup.
const (
	OSS3ScriptQuotaReserve  = "oss3:quota_reserve"
	OSS3ScriptQuotaCommit   = "oss3:quota_commit"
	OSS3ScriptQuotaRollback = "oss3:quota_rollback"
	OSS3ScriptRateLimit     = "oss3:rate_limit_check"
	OSS3ScriptLockAcquire   = "oss3:lock_acquire"
	OSS3ScriptLockExtend    = "oss3:lock_extend"
	OSS3ScriptLockRelease   = "oss3:lock_release"
)

// Lua script source code.
var (
	// luaQuotaReserve atomically reserves quota for an upload.
	// KEYS[1] = oss3:quota:{user_id} (current quota counter)
	// KEYS[2] = oss3:quota:pending:{user_id}:{request_id} (pending reservation)
	// ARGV[1] = amount (bytes to reserve)
	// ARGV[2] = maxQuota (user quota limit)
	// ARGV[3] = pendingTTL (seconds, e.g. 300)
	// Returns: 1 on success, 0 if quota exceeded
	luaQuotaReserve = `
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local pending = tonumber(redis.call('GET', KEYS[2]) or '0')
local amount  = tonumber(ARGV[1])
local maxQ    = tonumber(ARGV[2])
local ttl     = tonumber(ARGV[3])

if (current + pending + amount) > maxQ then
    return 0
end

redis.call('SET', KEYS[2], amount, 'EX', ttl)
return 1
`

	// luaQuotaCommit atomically commits a pending quota reservation.
	// KEYS[1] = oss3:quota:{user_id}
	// KEYS[2] = oss3:quota:pending:{user_id}:{request_id}
	// ARGV[1] = amount (bytes to commit, must not exceed pending)
	// Returns: 1 on success, 0 if pending key missing, -1 if amount exceeds pending
	luaQuotaCommit = `
local pendingVal = redis.call('GET', KEYS[2])
if not pendingVal then
    return 0
end

local reserved = tonumber(pendingVal)
local amount = tonumber(ARGV[1])
if amount > reserved then
    return -1
end

redis.call('INCRBY', KEYS[1], amount)
redis.call('DEL', KEYS[2])
return 1
`

	// luaQuotaRollback releases a pending quota reservation.
	// KEYS[1] = oss3:quota:pending:{user_id}:{request_id}
	// Returns: 1 if deleted, 0 if already gone
	luaQuotaRollback = `
return redis.call('DEL', KEYS[1])
`

	// luaRateLimitCheck atomically checks and records a request in the sliding window.
	// KEYS[1] = oss3:rate:{user_id} (ZSET: member=requestID, score=timestamp)
	// ARGV[1] = now (float, current unix timestamp)
	// ARGV[2] = windowStart (float, now - 60)
	// ARGV[3] = maxRequests (int)
	// ARGV[4] = requestID (unique per request)
	// Returns: current count (int)
	luaRateLimitCheck = `
local now         = tonumber(ARGV[1])
local windowStart = tonumber(ARGV[2])
local maxReqs     = tonumber(ARGV[3])
local reqID       = ARGV[4]

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', windowStart)

local count = redis.call('ZCARD', KEYS[1])

if count >= maxReqs then
    return count
end

redis.call('ZADD', KEYS[1], now, reqID)
redis.call('EXPIRE', KEYS[1], 120)

return count + 1
`

	// luaLockAcquire acquires a distributed lock.
	// KEYS[1] = oss3:lock:{resource}
	// ARGV[1] = holderUUID
	// ARGV[2] = ttlSeconds
	// Returns: 1 if acquired, 0 if held by another
	luaLockAcquire = `
local holder = ARGV[1]
local ttl    = tonumber(ARGV[2])

local current = redis.call('GET', KEYS[1])
if current and current ~= holder then
    return 0
end

redis.call('SET', KEYS[1], holder, 'EX', ttl)
return 1
`

	// luaLockExtend extends the TTL of a held lock.
	// KEYS[1] = oss3:lock:{resource}
	// ARGV[1] = holderUUID
	// ARGV[2] = newTTL
	// Returns: 1 if extended, 0 if lock lost
	luaLockExtend = `
local holder = ARGV[1]
local ttl    = tonumber(ARGV[2])

local current = redis.call('GET', KEYS[1])
if current ~= holder then
    return 0
end

redis.call('EXPIRE', KEYS[1], ttl)
return 1
`

	// luaLockRelease releases a distributed lock.
	// KEYS[1] = oss3:lock:{resource}
	// ARGV[1] = holderUUID
	// Returns: 1 if released, 0 if lock already expired or held by another
	luaLockRelease = `
local holder = ARGV[1]

local current = redis.call('GET', KEYS[1])
if current ~= holder then
    return 0
end

return redis.call('DEL', KEYS[1])
`
)

// RegisterOSS3Scripts loads all OSS3 Lua scripts into Redis and caches their SHA hashes.
// Call once at startup (after Redis connection is ready).
func RegisterOSS3Scripts(rdb *redis.Client) map[string]*redis.Script {
	scripts := map[string]*redis.Script{
		OSS3ScriptQuotaReserve:  redis.NewScript(luaQuotaReserve),
		OSS3ScriptQuotaCommit:   redis.NewScript(luaQuotaCommit),
		OSS3ScriptQuotaRollback: redis.NewScript(luaQuotaRollback),
		OSS3ScriptRateLimit:     redis.NewScript(luaRateLimitCheck),
		OSS3ScriptLockAcquire:   redis.NewScript(luaLockAcquire),
		OSS3ScriptLockExtend:    redis.NewScript(luaLockExtend),
		OSS3ScriptLockRelease:   redis.NewScript(luaLockRelease),
	}
	return scripts
}
