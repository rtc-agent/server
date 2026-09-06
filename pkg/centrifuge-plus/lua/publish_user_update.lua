-- publish_user_update.lua
-- Publish user_update to PUB/SUB with a pre-allocated user_update offset.
-- Ensures epoch exists (SETNX) and uses it consistently.
-- No epoch verification against meta key — epoch is per-channel, set once.
--
-- KEYS: [epoch_key, result_key]
--   epoch_key   -> channel:epoch:{channel}
--   result_key  -> idempotency cache key (empty string = no idempotency)
--
-- ARGV: [default_epoch, pubsub_channel, offset, message_payload,
--        publish_command, result_key_expire, traceparent]
--   default_epoch      -> epoch to SETNX if not exists
--   pubsub_channel     -> PUB/SUB channel name
--   offset             -> user_update offset (pre-allocated)
--   message_payload    -> data to publish
--   publish_command    -> "publish" or "spublish"
--   result_key_expire  -> idempotency cache TTL in seconds (empty = no idempotency)
--   traceparent        -> W3C traceparent (empty = no tracing)

local epoch_key = KEYS[1]
local result_key = KEYS[2]

local default_epoch = ARGV[1]
local pubsub_channel = ARGV[2]
local offset = tonumber(ARGV[3])
local message_payload = ARGV[4]
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

-- Ensure epoch exists (SETNX — only the first call sets it)
local epoch = redis.call("GET", epoch_key)
if epoch == false then
    redis.call("SET", epoch_key, default_epoch)
    epoch = default_epoch
end

-- Publish to PUB/SUB channel
-- Format: __p1:{offset}:{epoch}:{data_len}__{data}[__tp:{traceparent}]
if pubsub_channel ~= '' then
    local data_len = #message_payload
    local payload = "__p1:" .. offset .. ":" .. epoch .. ":" .. data_len .. "__" .. message_payload
    if traceparent ~= '' then
        payload = payload .. "__tp:" .. traceparent
    end
    redis.call(publish_command, pubsub_channel, payload)
end

-- Cache idempotency result
if result_key ~= '' and result_key_expire ~= '' then
    redis.call("hset", result_key, "e", epoch, "s", offset)
    redis.call("expire", result_key, result_key_expire)
end

return { tostring(offset), epoch, "0" }
