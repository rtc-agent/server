// Package cache manages all Redis Lua scripts centrally.
//
// Conventions:
//   - All Lua scripts are registered as *redis.NewScript in this file.
//   - Business code must not construct Lua strings directly; use this package to obtain registered script objects.
//   - When adding a new script, add a variable here with a brief comment (documenting KEYS/ARGV conventions and return values).
//   - go-redis automatically handles SCRIPT LOAD + EVALSHA caching; no manual management needed.
package cache

import "github.com/redis/go-redis/v9"

// GetDel atomically gets and deletes a key.
//
//	KEYS[1] = target key
//	Returns: the key's value (string), nil if it does not exist.
//
// Use case: one-time consumption of OAuth2 state after validation, preventing replay attacks.
// Compared to Redis 6.2 built-in GETDEL, this script is compatible with older Redis versions.
var GetDel = redis.NewScript(`
local val = redis.call('GET', KEYS[1])
if val then
    redis.call('DEL', KEYS[1])
end
return val
`)

// SetNX atomically sets a key (only when it does not exist), with TTL.
//
//	KEYS[1] = target key
//	ARGV[1] = value
//	ARGV[2] = TTL (seconds)
//	Returns: 1 if set successfully, 0 if key already exists.
//
// Use case: idempotent markers, distributed locks, etc.
var SetNX = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
    redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
    return 1
else
    return 0
end
`)

// BatchIncrOffset atomically increments offset counters for multiple channels in a batch.
//
//	KEYS = [channel1, channel2, ...]  channel offset counter key list
//	ARGV = [count1, count2, ...]      increment for each channel (corresponds to KEYS one-to-one)
//	Returns: list of max offset values after increment for each channel (same order as KEYS).
//
// Use case: generate consecutive offsets for multiple channels in a single Redis call, reducing network round trips.
// Single-key increment can also use this script (pass 1 KEYS and corresponding ARGV increment).
var BatchIncrOffset = redis.NewScript(`
local results = {}
for i, key in ipairs(KEYS) do
    local count = tonumber(ARGV[i])
    results[i] = redis.call('INCRBY', key, count)
end
return results
`)

// WorkerRegister registers a new Worker or re-registers an expired Worker.
//
//	KEYS[1] = worker:{workerID}
//	KEYS[2] = workers:active
//	KEYS[3] = worker:{workerID}:sessions
//	ARGV[1] = workerID, ARGV[2] = host, ARGV[3] = version
//	ARGV[4] = current_timestamp, ARGV[5] = ttl_seconds
//
// Returns: {ok = 1} on success; returns error if Worker is already running.
var WorkerRegister = redis.NewScript(`
local worker_key = KEYS[1]
local active_key = KEYS[2]
local sessions_key = KEYS[3]
local worker_id = ARGV[1]

if redis.call('EXISTS', worker_key) == 1 then
    local status = redis.call('HGET', worker_key, 'status')
    if status == 'running' then
        return {err = "Worker already registered"}
    end
    redis.call('DEL', worker_key)
    redis.call('DEL', sessions_key)
end

redis.call('HSET', worker_key,
    'status', 'running', 'started_at', ARGV[4], 'last_heartbeat', ARGV[4],
    'session_count', 0, 'host', ARGV[2], 'version', ARGV[3])
redis.call('EXPIRE', worker_key, tonumber(ARGV[5]))
redis.call('SADD', active_key, worker_id)
return {ok = 1}
`)

// WorkerHeartbeat updates Worker heartbeat and session count, and refreshes TTL.
//
//	KEYS[1] = worker:{workerID}, KEYS[2] = worker:{workerID}:sessions
//	ARGV[1] = timestamp, ARGV[2] = session_count
//	ARGV[3] = ttl_seconds
//
// Returns: {ok = 1} on success; returns error if Worker does not exist.
var WorkerHeartbeat = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
    return {err = "Worker not found"}
end
redis.call('HSET', KEYS[1],
    'last_heartbeat', ARGV[1], 'session_count', ARGV[2])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[3]))
return {ok = 1}
`)

// WorkerDeregister deregisters a Worker, cleans up all its keys, and returns the session list that needs reassignment.
//
//	KEYS[1] = worker:{workerID}, KEYS[2] = workers:active
//	KEYS[3] = worker:{workerID}:sessions, KEYS[4] = worker:{workerID}:queue
//	ARGV[1] = workerID
//
// Returns: list of sessions that need reassignment.
var WorkerDeregister = redis.NewScript(`
local sessions = redis.call('HKEYS', KEYS[3])
redis.call('SREM', KEYS[2], ARGV[1])
redis.call('DEL', KEYS[1])
redis.call('DEL', KEYS[3])
redis.call('DEL', KEYS[4])
return sessions
`)

// SessionAssign assigns a Session to a Worker; if already assigned to an active Worker, keeps the current assignment.
//
//	KEYS[1] = session:affinity, KEYS[2] = worker:{targetWorkerID}:sessions
//	KEYS[3] = workers:active
//	ARGV[1] = sessionID, ARGV[2] = targetWorkerID, ARGV[3] = timestamp
//
// Returns: {assigned, worker, reassigned}.
var SessionAssign = redis.NewScript(`
local current_worker = redis.call('HGET', KEYS[1], ARGV[1])
if current_worker and current_worker ~= '' then
    if redis.call('SISMEMBER', KEYS[3], current_worker) == 1 then
        return {assigned = 1, worker = current_worker, reassigned = 0}
    end
end

redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
return {assigned = 1, worker = ARGV[2], reassigned = 1}
`)

// SessionReassign migrates a Session from a failed Worker to a new Worker.
//
//	KEYS[1] = session:affinity
//	KEYS[2] = worker:{deadWorkerID}:sessions
//	KEYS[3] = worker:{newWorkerID}:sessions
//	ARGV[1] = sessionID, ARGV[2] = newWorkerID, ARGV[3] = timestamp
//
// Returns: {reassigned = 1, worker = newWorkerID}.
var SessionReassign = redis.NewScript(`
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[3])
return {reassigned = 1, worker = ARGV[2]}
`)

// TurnEnqueue enqueues a Turn message into the Worker's Stream queue.
//
//	KEYS[1] = worker:{workerID}:queue
//	KEYS[2] = session:affinity
//	KEYS[3] = worker:{workerID}
//	KEYS[4] = worker:{workerID}:sessions
//	ARGV[1] = workerID, ARGV[2] = sessionID, ARGV[3] = turnID
//	ARGV[4] = messageID, ARGV[5] = userID, ARGV[6] = deviceID
//	ARGV[7] = content, ARGV[8] = created_at
//
// Returns: {enqueued = 1, stream_id = ...}.
var TurnEnqueue = redis.NewScript(`
local assigned_worker = redis.call('HGET', KEYS[2], ARGV[2])
if assigned_worker == '' or not assigned_worker then
    redis.call('HSET', KEYS[2], ARGV[2], ARGV[1])
    redis.call('HSET', KEYS[4], ARGV[2], ARGV[8])
elseif assigned_worker ~= ARGV[1] then
    return {err = "Session not assigned to this worker"}
end

if redis.call('EXISTS', KEYS[3]) == 0 then
    return {err = "Worker not active"}
end

local stream_id = redis.call('XADD', KEYS[1], '*',
    'session_id', ARGV[2], 'turn_id', ARGV[3], 'message_id', ARGV[4],
    'user_id', ARGV[5], 'device_id', ARGV[6], 'content', ARGV[7],
    'created_at', ARGV[8])

return {enqueued = 1, stream_id = stream_id}
`)

// AppendChunk atomically appends a streaming chunk to a Redis List and refreshes TTL.
//
//	KEYS[1] = message:stream:{messageID}
//	ARGV[1] = ttl_seconds, ARGV[2] = chunk
//	Returns: current list length (RPUSH return value).
//
// Use case: during streaming message generation, each chunk is atomically appended to the List;
// after the last chunk arrives, all chunks are read, concatenated, written to DB, then the key is deleted.
var AppendChunk = redis.NewScript(`
local len = redis.call('RPUSH', KEYS[1], ARGV[2])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[1]))
return len
`)

// UpdateMaxStreamID atomically updates the Stream consumption position (only when new ID > current ID).
//
//	KEYS[1] = key storing lastID
//	ARGV[1] = new Stream ID (format "timestamp-sequence")
//	Returns: 1 if updated, 0 if not updated (current ID >= new ID).
//
// Use case: BackgroundRunner persists consumption position, resumes from last position after restart.
// Lua script guarantees atomicity and monotonic increase, preventing concurrent writes from causing position regression.
var UpdateMaxStreamID = redis.NewScript(`
local key = KEYS[1]
local new_id = ARGV[1]
local current = redis.call('GET', key)

if not current then
    redis.call('SET', key, new_id)
    return 1
end

-- Parse stream IDs: "timestamp-sequence"
local cur_parts = {}
for part in string.gmatch(current, "([^%-]+)") do
    table.insert(cur_parts, tonumber(part))
end

local new_parts = {}
for part in string.gmatch(new_id, "([^%-]+)") do
    table.insert(new_parts, tonumber(part))
end

-- Compare: update only if new > current
-- Stream IDs are compared by timestamp first, then sequence
if new_parts[1] > cur_parts[1] or
   (new_parts[1] == cur_parts[1] and new_parts[2] > cur_parts[2]) then
    redis.call('SET', key, new_id)
    return 1
else
    return 0
end
`)

// InterruptSetPublish atomically executes SET + PUBLISH, ensuring consistency between answer storage and notification.
//
//	KEYS[1] = answer key (interrupt:answer:{sessionID}:{interruptID})
//	KEYS[2] = pub/sub channel (interrupt:channel:{sessionID}:{interruptID})
//	ARGV[1] = answer content
//	ARGV[2] = TTL (seconds)
//	Returns: number of subscribers that received the PUBLISH (int).
//
// Atomicity guarantee: the inconsistent state of SET succeeding but PUBLISH failing is no longer possible.
// Subscriber tolerance logic (SUBSCRIBE before GET) is still retained as a safety net.
var InterruptSetPublish = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[1], 'EX', tonumber(ARGV[2]))
return redis.call('PUBLISH', KEYS[2], ARGV[1])
`)

// BatchComplete atomically completes one RTC in a batch resume: stores result, removes from pending set, returns remaining count.
//
//	KEYS[1] = rtc:batch:pending:{turnID}   pending RTC set
//	KEYS[2] = rtc:batch:results:{turnID}   result storage Hash
//	ARGV[1] = RTC ID
//	ARGV[2] = RTC result string
//	ARGV[3] = TTL (seconds)
//
//	Returns: -1 if batch key does not exist (TTL expired or not created), >=0 is the remaining member count after removal.
//
//	Atomicity guarantee: HSET + SREM + SCARD + EXPIRE execute in a single script, avoiding races when multiple RTCs complete concurrently.
//	When return value is 0, the caller knows all RTCs have completed and can trigger batch resume.
var BatchComplete = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
    return -1
end
redis.call('HSET', KEYS[2], ARGV[1], ARGV[2])
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[3]))
redis.call('SREM', KEYS[1], ARGV[1])
return redis.call('SCARD', KEYS[1])
`)
