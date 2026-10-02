package cache

import "github.com/redis/go-redis/v9"

// OSS3 Lua script names, registered at startup.
const (
	OSS3ScriptQuotaReserve  = "oss3:quota_reserve"
	OSS3ScriptQuotaCommit   = "oss3:quota_commit"
	OSS3ScriptQuotaRollback = "oss3:quota_rollback"
	OSS3ScriptQuotaAdjust   = "oss3:quota_adjust"
	OSS3ScriptRateLimit     = "oss3:rate_limit_check"
	OSS3ScriptLockAcquire   = "oss3:lock_acquire"
	OSS3ScriptLockExtend    = "oss3:lock_extend"
	OSS3ScriptLockRelease   = "oss3:lock_release"
)

// Lua script source code.
// All scripts follow atomic execution semantics to ensure data consistency in distributed environments.
var (
	// luaQuotaReserve atomically reserves quota for an upload (two-phase allocation).
	//
	//	KEYS[1] = oss3:quota:{user_id}              (current committed quota counter)
	//	KEYS[2] = oss3:quota:pending:{uid}:{reqID}  (this request's pending reservation, with TTL)
	//	KEYS[3] = oss3:quota:pending_agg:{uid}      (ZSET aggregating ALL pending reservations;
	//	                                            member=requestID, score=amount)
	//	ARGV[1] = amount            (bytes to reserve)
	//	ARGV[2] = maxQuota          (user quota limit)
	//	ARGV[3] = pendingTTL        (seconds, e.g. 300)
	//	ARGV[4] = requestID         (unique request identifier, used as ZSET member)
	//	ARGV[5] = pendingKeyPrefix  (e.g. "oss3:quota:pending:{uid}:", for constructing
	//	                             other requests' pending keys during aggregation scan)
	//
	// Returns: 1 on success, 0 if quota exceeded.
	// Idempotent: if pending key already exists with same amount, returns 1 without re-reserving.
	//
	// Aggregation: The ZSET in KEYS[3] tracks ALL pending reservations for this user.
	// This solves the multi-concurrent-reserve problem: when multiple Reserve calls happen
	// in parallel, each call sees the SUM of all pending amounts and correctly enforces quota.
	//
	// Stale entry cleanup: If a pending key has expired (TTL), its ZSET entry becomes stale
	// (score > 0 but no backing key). The script detects and cleans up stale entries when
	// encountered during ZSET iteration, keeping the aggregation accurate.
	luaQuotaReserve = `
local current   = tonumber(redis.call('GET', KEYS[1]) or '0')
local amount    = tonumber(ARGV[1])
local maxQ      = tonumber(ARGV[2])
local ttl       = tonumber(ARGV[3])
local requestID = ARGV[4]
local prefix    = ARGV[5]
local pendingKey = KEYS[2]
local aggKey     = KEYS[3]

-- Idempotency check: if this request's pending key exists with same amount, return success.
local existingPending = redis.call('GET', pendingKey)
if existingPending then
    local existingAmount = tonumber(existingPending)
    if existingAmount == amount then
        return 1
    end
end

-- Sum ALL pending amounts from the aggregation ZSET, with lazy cleanup of stale entries.
-- A stale entry is one whose pending key has expired (TTL) but whose ZSET score remains.
local totalPending = 0
local cursor = "0"
repeat
    local result = redis.call('ZSCAN', aggKey, cursor)
    cursor = result[1]
    local entries = result[2]
    for i = 1, #entries, 2 do
        local member = entries[i]
        -- Construct the pending key for this member using the prefix.
        local memberPendingKey = prefix .. member
        local memberVal = redis.call('GET', memberPendingKey)
        if not memberVal then
            -- Stale entry: the pending key expired. Clean it up.
            redis.call('ZREM', aggKey, member)
        else
            -- Live entry: use the actual key value (authoritative source).
            totalPending = totalPending + tonumber(memberVal)
        end
    end
until cursor == "0"

-- Quota check: committed + all_pending + new_amount must not exceed limit.
if (current + totalPending + amount) > maxQ then
    return 0
end

-- Record the pending reservation (with TTL for safety) and add to aggregation ZSET.
redis.call('SET', pendingKey, amount, 'EX', ttl)
redis.call('ZADD', aggKey, amount, requestID)
return 1
`

	// luaQuotaCommit atomically commits a pending quota reservation.
	//
	//	KEYS[1] = oss3:quota:{user_id}              (current quota counter)
	//	KEYS[2] = oss3:quota:pending:{uid}:{reqID}  (pending reservation)
	//	KEYS[3] = oss3:quota:pending_agg:{uid}      (ZSET aggregating ALL pending reservations)
	//	ARGV[1] = amount   (bytes to commit, must not exceed pending)
	//	ARGV[2] = requestID (used to remove the entry from the aggregation ZSET)
	//
	// Returns: 1 on success, 0 if pending key missing (expired or already committed),
	//          -1 if amount exceeds pending (caller bug).
	//
	// Use case: after successful upload, commit reserved quota to actual usage counter.
	//          Also removes the entry from the aggregation ZSET to keep it accurate.
	luaQuotaCommit = `
local pendingVal = redis.call('GET', KEYS[2])
if not pendingVal then
    -- Pending key gone (expired). Clean up the ZSET entry too.
    redis.call('ZREM', KEYS[3], ARGV[2])
    return 0
end

local reserved = tonumber(pendingVal)
local amount = tonumber(ARGV[1])
if amount > reserved then
    return -1
end

redis.call('INCRBY', KEYS[1], amount)
redis.call('DEL', KEYS[2])
redis.call('ZREM', KEYS[3], ARGV[2])
return 1
`

	// luaQuotaRollback releases a pending quota reservation.
	//
	//	KEYS[1] = oss3:quota:pending:{uid}:{reqID}  (pending reservation)
	//	KEYS[2] = oss3:quota:pending_agg:{uid}      (ZSET aggregating ALL pending reservations)
	//	ARGV[1] = requestID (used to remove the entry from the aggregation ZSET)
	//
	// Returns: 1 if deleted, 0 if already gone (expired or committed).
	//          Always removes the ZSET entry (even if pending key was already gone).
	//
	// Use case: upload failure or cancellation releases reserved quota.
	luaQuotaRollback = `
local result = redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[1])
return result
`

	// luaQuotaAdjust adjusts the committed quota counter by a delta (positive or negative).
	//
	//	KEYS[1] = oss3:quota:{user_id}  (current quota counter)
	//	ARGV[1] = delta  (positive = add, negative = subtract)
	//
	// Returns: 1 on success, 0 if would go negative (caller bug or race).
	//
	// Use case: instant upload detects duplicate content in multipart uploads,
	// releases quota for the duplicate portion.
	luaQuotaAdjust = `
local delta = tonumber(ARGV[1])
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local newVal = current + delta

if newVal < 0 then
    return 0
end

redis.call('SET', KEYS[1], newVal)
return 1
`

	// luaRateLimitCheck atomically checks and records a request in the sliding window.
	//
	//	KEYS[1] = oss3:rate:{user_id}  (ZSET: member=requestID, score=timestamp)
	//	ARGV[1] = now           (float, current unix timestamp)
	//	ARGV[2] = windowStart   (float, now - 60)
	//	ARGV[3] = maxRequests   (int)
	//	ARGV[4] = requestID     (unique per request)
	//
	// Returns: current count (int), including this request if allowed.
	//
	// Use case: per-user S3 API rate limiting with sliding window algorithm.
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

	// luaLockAcquire acquires a distributed lock (reentrant for same holder).
	//
	//	KEYS[1] = oss3:lock:{resource}  (lock key)
	//	ARGV[1] = holderUUID  (unique holder identifier)
	//	ARGV[2] = ttlSeconds  (lock TTL)
	//
	// Returns: 1 if acquired, 0 if held by another holder.
	//
	// Use case: cleanup task lock, ensuring single-writer semantics across nodes.
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
	//
	//	KEYS[1] = oss3:lock:{resource}  (lock key)
	//	ARGV[1] = holderUUID  (must match current holder)
	//	ARGV[2] = newTTL      (new TTL in seconds)
	//
	// Returns: 1 if extended, 0 if lock lost (expired or stolen).
	//
	// Use case: long-running cleanup task extends lock to prevent premature expiration.
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

	// luaLockRelease releases a distributed lock (only if still held by caller).
	//
	//	KEYS[1] = oss3:lock:{resource}  (lock key)
	//	ARGV[1] = holderUUID  (must match current holder)
	//
	// Returns: 1 if released, 0 if lock already expired or held by another.
	//
	// Use case: cleanup task completion releases lock for next acquisition.
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
//
// Returns: map from script name (e.g., OSS3ScriptQuotaReserve) to *redis.Script.
// go-redis automatically handles SCRIPT LOAD + EVALSHA caching; no manual management needed.
func RegisterOSS3Scripts(rdb *redis.Client) map[string]*redis.Script {
	scripts := map[string]*redis.Script{
		OSS3ScriptQuotaReserve:  redis.NewScript(luaQuotaReserve),
		OSS3ScriptQuotaCommit:   redis.NewScript(luaQuotaCommit),
		OSS3ScriptQuotaRollback: redis.NewScript(luaQuotaRollback),
		OSS3ScriptQuotaAdjust:   redis.NewScript(luaQuotaAdjust),
		OSS3ScriptRateLimit:     redis.NewScript(luaRateLimitCheck),
		OSS3ScriptLockAcquire:   redis.NewScript(luaLockAcquire),
		OSS3ScriptLockExtend:    redis.NewScript(luaLockExtend),
		OSS3ScriptLockRelease:   redis.NewScript(luaLockRelease),
	}
	return scripts
}
