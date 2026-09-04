-- publish_with_offset.lua
-- Publish to PUB/SUB using a pre-allocated offset.
-- No HINCRBY, no XADD, no Asynq.
--
-- KEYS: [meta_key, result_key]
--   meta_key    -> {prefix}:meta:{channel}
--   result_key  -> {prefix}:result:{channel} (empty string = no idempotency)
--
-- ARGV: [message_payload, channel, offset, epoch,
--        publish_command, result_key_expire, traceparent]
--   message_payload    -> data to publish
--   channel            -> PUB/SUB channel name
--   offset             -> pre-allocated offset
--   epoch              -> pre-allocated epoch
--   publish_command    -> "publish" or "spublish"
--   result_key_expire  -> idempotency cache TTL (empty = no idempotency)
--   traceparent        -> W3C traceparent (empty = no tracing)

local meta_key = KEYS[1]
local result_key = KEYS[2]

local message_payload = ARGV[1]
local channel = ARGV[2]
local offset = tonumber(ARGV[3])
local epoch = ARGV[4]
local publish_command = ARGV[5]
local result_key_expire = ARGV[6]
local traceparent = ARGV[7]

-- Idempotency check
if result_key ~= '' and result_key_expire ~= '' then
    local cached_result = redis.call("hmget", result_key, "e", "s")
    local result_epoch, result_offset = cached_result[1], cached_result[2]
    if result_epoch ~= false then
        return { result_offset, result_epoch, "1" }
    end
end

-- Verify epoch matches meta (prevent stale pre-allocated offsets from being used)
local current_epoch = redis.call("hget", meta_key, "e")
if current_epoch ~= epoch then
    return { "-1", current_epoch, "0" }  -- epoch mismatch, reject publish
end

-- Publish to PUB/SUB channel
-- Format: __p1:{offset}:{epoch}:{data_len}__{data}[__tp:{traceparent}]
-- Length-prefix encoding prevents message content from interfering with meta parsing.
if channel ~= '' then
    local data_len = #message_payload
    local payload = "__p1:" .. offset .. ":" .. epoch .. ":" .. data_len .. "__" .. message_payload
    if traceparent ~= '' then
        payload = payload .. "__tp:" .. traceparent
    end
    redis.call(publish_command, channel, payload)
end

-- Cache idempotency result
if result_key ~= '' and result_key_expire ~= '' then
    redis.call("hset", result_key, "e", epoch, "s", offset)
    redis.call("expire", result_key, result_key_expire)
end

return { tostring(offset), epoch, "0" }
