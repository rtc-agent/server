package rtcqueue

import "github.com/redis/go-redis/v9"

// claimScript atomically claims a session: checks the lock, inspects the
// queue, sets the lock, pops the highest-priority work, and updates its
// status to processing. Also stores the active work ID in session:active
// so CancelSession can find it later. Returns [workID] or nil when there
// is nothing to claim.
//
// KEYS[1] = session lock
// KEYS[2] = session queue (zset)
// KEYS[3] = session active work pointer ("session:active:<sessionID>")
// ARGV[1] = worker_id
// ARGV[2] = lock ttl seconds
// ARGV[3] = now (unix seconds)
var claimScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[1]) == 1 then
    return nil
end
if redis.call("ZCARD", KEYS[2]) == 0 then
    return nil
end
if redis.call("SET", KEYS[1], ARGV[1], "NX", "EX", tonumber(ARGV[2])) == false then
    return nil
end
local popped = redis.call("ZPOPMIN", KEYS[2], 1)
if #popped == 0 then
    return nil
end
local work_id = popped[1]
redis.call("HSET", "work:" .. work_id,
    "status", "processing",
    "worker_id", ARGV[1],
    "claimed_at", ARGV[3],
    "updated_at", ARGV[3])
redis.call("SET", KEYS[3], work_id)
return {work_id}
`)

// publishScript atomically persists a Work item, enqueues it in the
// session's priority queue, and notifies workers.
//
// KEYS[1] = work hash key ("work:<work_id>")
// KEYS[2] = session queue (zset)
// ARGV[1]  = work_id
// ARGV[2]  = session_id
// ARGV[3]  = data
// ARGV[4]  = priority (signed integer string)
// ARGV[5]  = created_at (unix seconds)
// ARGV[6]  = updated_at (unix seconds)
// ARGV[7]  = channel name for notification
// ARGV[8]  = session_id notification payload
var publishScript = redis.NewScript(`
local work_id = KEYS[1]:sub(6)
redis.call("HSET", KEYS[1],
    "id",         ARGV[1],
    "session_id", ARGV[2],
    "data",       ARGV[3],
    "priority",   ARGV[4],
    "status",     "pending",
    "created_at", ARGV[5],
    "updated_at", ARGV[6],
    "worker_id",  "",
    "claimed_at", "0")
redis.call("ZADD", KEYS[2], 0 - tonumber(ARGV[4]), work_id)
redis.call("PUBLISH", ARGV[7], ARGV[8])
return 1
`)

// completeScript marks a Work item completed and releases the session
// lock in one atomic step. Also clears the session:active pointer.
// If the session's queue still has pending work items, publishes a
// session:new notification so workers can claim the next item.
//
// KEYS[1] = work hash key
// ARGV[1] = now (unix seconds)
// ARGV[2] = session:new channel name
var completeScript = redis.NewScript(`
local sid = redis.call("HGET", KEYS[1], "session_id")
if not sid then
    return 0
end
redis.call("HSET", KEYS[1], "status", "completed", "updated_at", ARGV[1])
redis.call("DEL", "session:lock:" .. sid)
redis.call("DEL", "session:active:" .. sid)
-- Check if there are pending work items in the queue
local queue_key = "queue:session:" .. sid
if redis.call("ZCARD", queue_key) > 0 then
    -- Notify workers to claim the next item
    redis.call("PUBLISH", ARGV[2], sid)
end
return 1
`)

// cancelScript cancels a Work item. If it was pending, it is removed
// from the session queue. A cancel notification is always published so
// any worker holding the session lock can abort. The lock is released.
// Also clears the session:active pointer.
//
// KEYS[1] = work hash key
// ARGV[1] = now (unix seconds)
// ARGV[2] = cancel message payload (JSON)
var cancelScript = redis.NewScript(`
local sid = redis.call("HGET", KEYS[1], "session_id")
if not sid then
    return 0
end
local status = redis.call("HGET", KEYS[1], "status")
if status == "completed" or status == "cancelled" then
    return -1
end
if status == "pending" then
    redis.call("ZREM", "queue:session:" .. sid, KEYS[1]:sub(6))
end
redis.call("HSET", KEYS[1], "status", "cancelled", "updated_at", ARGV[1])
-- notify subscribers BEFORE releasing the lock: otherwise a competing
-- worker in a tight claimScript retry loop can acquire the lock between
-- DEL and PUBLISH and start processing while the cancel notification is
-- still in flight. Subscribers must see the cancel first.
redis.call("PUBLISH", "session:cancel:" .. sid, ARGV[2])
redis.call("DEL", "session:lock:" .. sid)
redis.call("DEL", "session:active:" .. sid)
return 1
`)

// cancelSessionScript removes all pending work items from a session's queue
// atomically, marks them as cancelled, and returns their work IDs.
//
// KEYS[1] = session queue (zset) "queue:session:<sessionID>"
// ARGV[1] = now (unix seconds)
// Returns: array of removed work IDs (may be empty).
var cancelSessionScript = redis.NewScript(`
local ids = redis.call("ZRANGE", KEYS[1], 0, -1)
if #ids > 0 then
    redis.call("DEL", KEYS[1])
    for i, wid in ipairs(ids) do
        redis.call("HSET", "work:" .. wid, "status", "cancelled", "updated_at", ARGV[1])
    end
end
return ids
`)

// cancelSessionActiveScript cancels the work item currently being processed
// for a session (if any). It reads the session:active pointer, publishes a
// cancel notification, and releases both the session lock and the active
// pointer. Publish happens BEFORE releasing the lock to prevent a race where
// a competing worker acquires the lock between DEL and PUBLISH.
//
// KEYS[1] = session lock ("session:lock:<sessionID>")
// KEYS[2] = session active pointer ("session:active:<sessionID>")
// KEYS[3] = session cancel channel ("session:cancel:<sessionID>")
// ARGV[1] = reason
// ARGV[2] = now (unix seconds)
// Returns: workID that was cancelled, or nil if no active work.
var cancelSessionActiveScript = redis.NewScript(`
local work_id = redis.call("GET", KEYS[2])
if not work_id then
    return nil
end
redis.call("DEL", KEYS[2])
local msg = cjson.encode({
    work_id = work_id,
    reason = ARGV[1],
    timestamp = tonumber(ARGV[2])
})
-- publish BEFORE releasing the lock (see cancelScript for rationale)
redis.call("PUBLISH", KEYS[3], msg)
redis.call("DEL", KEYS[1])
return work_id
`)

// renewLockScript atomically refreshes the session lock TTL only if it
// is still owned by the calling worker. The read+compare+expire dance
// MUST be atomic — otherwise the lock can expire between GET and
// EXPIRE, be re-acquired by a different worker via claimScript, and
// the stale EXPIRE would silently extend the new owner's hold.
//
// KEYS[1] = session lock
// ARGV[1] = worker_id (expected owner)
// ARGV[2] = lock ttl seconds
// Returns 1 on success, 0 if the lock is missing or owned by someone
// else.
var renewLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
    return 0
end
return redis.call("EXPIRE", KEYS[1], tonumber(ARGV[2]))
`)
