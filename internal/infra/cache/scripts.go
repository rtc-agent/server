// Package redisscript 统一管理所有 Redis Lua 脚本。
//
// 规范：
//   - 所有 Lua 脚本以 *redis.NewScript 形式集中注册在此文件。
//   - 业务代码禁止直接拼接 Lua 字符串，必须通过本包获取已注册的脚本对象。
//   - 新增脚本时在此文件添加变量 + 简要注释（说明 KEYS/ARGV 约定和返回值）。
//   - go-redis 会自动对脚本做 SCRIPT LOAD + EVALSHA 缓存，无需手动管理。
package cache

import "github.com/redis/go-redis/v9"

// GetDel 原子获取并删除 key。
//
//	KEYS[1] = 目标 key
//	返回：key 的值（string），不存在时返回 nil。
//
// 用途：OAuth2 state 验证后一次性消费，防止重放攻击。
// 相比 Redis 6.2 内置 GETDEL，此脚本兼容更低版本 Redis。
var GetDel = redis.NewScript(`
local val = redis.call('GET', KEYS[1])
if val then
    redis.call('DEL', KEYS[1])
end
return val
`)

// SetNX 原子设置 key（仅在不存在时写入），带 TTL。
//
//	KEYS[1] = 目标 key
//	ARGV[1] = value
//	ARGV[2] = TTL（秒）
//	返回：1 表示设置成功，0 表示 key 已存在。
//
// 用途：幂等标记、分布式锁等场景。
var SetNX = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
    redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
    return 1
else
    return 0
end
`)

// BatchIncrOffset 批量原子递增多个频道的 offset 计数器。
//
//	KEYS = [channel1, channel2, ...]  频道 offset 计数器 key 列表
//	ARGV = [count1, count2, ...]      每个频道的增量（与 KEYS 一一对应）
//	返回：每个频道递增后的最大 offset 值列表（顺序与 KEYS 相同）。
//
// 用途：一次 Redis 调用为多个频道批量生成连续 offset，减少网络往返。
// 单 key 递增也可通过本脚本实现（KEYS 传 1 个、ARGV 传对应增量）。
var BatchIncrOffset = redis.NewScript(`
local results = {}
for i, key in ipairs(KEYS) do
    local count = tonumber(ARGV[i])
    results[i] = redis.call('INCRBY', key, count)
end
return results
`)

// WorkerRegister 注册一个新的 Worker 或重新注册已失效的 Worker。
//
//	KEYS[1] = worker:{workerID}
//	KEYS[2] = workers:active
//	KEYS[3] = worker:{workerID}:sessions
//	ARGV[1] = workerID, ARGV[2] = host, ARGV[3] = version
//	ARGV[4] = current_timestamp, ARGV[5] = ttl_seconds
//
// 返回：{ok = 1} 成功；若 Worker 已处于 running 状态则返回错误。
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

// WorkerHeartbeat 更新 Worker 心跳与会话计数，并刷新 TTL。
//
//	KEYS[1] = worker:{workerID}, KEYS[2] = worker:{workerID}:sessions
//	ARGV[1] = timestamp, ARGV[2] = session_count
//	ARGV[3] = ttl_seconds
//
// 返回：{ok = 1} 成功；若 Worker 不存在则返回错误。
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

// WorkerDeregister 注销 Worker，清理其所有 key，返回需要重新分配的 session 列表。
//
//	KEYS[1] = worker:{workerID}, KEYS[2] = workers:active
//	KEYS[3] = worker:{workerID}:sessions, KEYS[4] = worker:{workerID}:queue
//	ARGV[1] = workerID
//
// 返回：需要重新分配的 sessions 列表。
var WorkerDeregister = redis.NewScript(`
local sessions = redis.call('HKEYS', KEYS[3])
redis.call('SREM', KEYS[2], ARGV[1])
redis.call('DEL', KEYS[1])
redis.call('DEL', KEYS[3])
redis.call('DEL', KEYS[4])
return sessions
`)

// SessionAssign 将 Session 分配给 Worker，若已分配给活跃 Worker 则保持原状。
//
//	KEYS[1] = session:affinity, KEYS[2] = worker:{targetWorkerID}:sessions
//	KEYS[3] = workers:active
//	ARGV[1] = sessionID, ARGV[2] = targetWorkerID, ARGV[3] = timestamp
//
// 返回：{assigned, worker, reassigned}。
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

// SessionReassign 将 Session 从失效 Worker 迁移到新 Worker。
//
//	KEYS[1] = session:affinity
//	KEYS[2] = worker:{deadWorkerID}:sessions
//	KEYS[3] = worker:{newWorkerID}:sessions
//	ARGV[1] = sessionID, ARGV[2] = newWorkerID, ARGV[3] = timestamp
//
// 返回：{reassigned = 1, worker = newWorkerID}。
var SessionReassign = redis.NewScript(`
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[3])
return {reassigned = 1, worker = ARGV[2]}
`)

// TurnEnqueue 将一条 Turn 消息入队到 Worker 的 Stream 队列。
//
//	KEYS[1] = worker:{workerID}:queue
//	KEYS[2] = session:affinity
//	KEYS[3] = worker:{workerID}
//	KEYS[4] = worker:{workerID}:sessions
//	ARGV[1] = workerID, ARGV[2] = sessionID, ARGV[3] = turnID
//	ARGV[4] = messageID, ARGV[5] = userID, ARGV[6] = deviceID
//	ARGV[7] = content, ARGV[8] = created_at
//
// 返回：{enqueued = 1, stream_id = ...}。
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

// AppendChunk 原子追加流式 chunk 到 Redis List 并刷新 TTL。
//
//	KEYS[1] = message:stream:{messageID}
//	ARGV[1] = ttl_seconds, ARGV[2] = chunk
//	返回：list 当前长度（RPUSH 的返回值）。
//
// 用途：流式消息生成期间，每个 chunk 原子追加到 List；
// 最后一个 chunk 到达后，读取全部 chunks 拼接写入 DB，再删除该 key。
var AppendChunk = redis.NewScript(`
local len = redis.call('RPUSH', KEYS[1], ARGV[2])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[1]))
return len
`)

// UpdateMaxStreamID 原子更新 Stream 消费位置（仅当新 ID 大于当前 ID 时）。
//
//	KEYS[1] = 存储 lastID 的 key
//	ARGV[1] = 新的 Stream ID（格式 "timestamp-sequence"）
//	返回：1 表示更新成功，0 表示未更新（当前 ID >= 新 ID）。
//
// 用途：BackgroundRunner 持久化消费位置，重启后从上次位置继续。
// Lua 脚本保证原子性和单调递增，防止并发写入导致位置回退。
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

// InterruptSetPublish 原子执行 SET + PUBLISH，保证答案存储与通知的一致性。
//
//	KEYS[1] = answer key（interrupt:answer:{sessionID}:{interruptID}）
//	KEYS[2] = pub/sub channel（interrupt:channel:{sessionID}:{interruptID}）
//	ARGV[1] = answer 内容
//	ARGV[2] = TTL（秒）
//	返回：PUBLISH 的订阅者数量（int）
//
// 原子性保证：SET 成功但 PUBLISH 失败的不一致状态不再可能出现。
// 订阅方先 SUBSCRIBE 再 GET 的容错逻辑仍然保留，作为兜底。
var InterruptSetPublish = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[1], 'EX', tonumber(ARGV[2]))
return redis.call('PUBLISH', KEYS[2], ARGV[1])
`)
