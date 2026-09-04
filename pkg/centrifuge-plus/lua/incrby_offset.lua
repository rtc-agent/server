-- incrby_offset.lua
-- Batch pre-allocate offsets. Atomically executes multiple HINCRBY with per-request count.
--
-- KEYS: [meta_key_1, meta_key_2, ...]
--   Each channel's meta key ({prefix}:meta:{channel})
--
-- ARGV: [count_1, new_epoch_1, count_2, new_epoch_2, ...]
--   count_i     -> how many offsets to allocate for channel i (>= 1)
--   new_epoch_i -> epoch to set if the meta doesn't exist yet
--
-- Returns: [final_offset_1, epoch_1, final_offset_2, epoch_2, ...]
--   final_offset_i is the HIGHEST allocated offset for channel i.
--   The N allocated offsets are [final - count + 1, ..., final].

if #KEYS * 2 ~= #ARGV then
    return redis.error_reply("ARGV must have exactly 2 elements per KEY (count + epoch): keys=" .. #KEYS .. " argv=" .. #ARGV)
end

local results = {}
for i = 1, #KEYS do
    local meta_key = KEYS[i]
    local count = tonumber(ARGV[i * 2 - 1])
    local new_epoch = ARGV[i * 2]

    if count == nil or count < 1 then
        return redis.error_reply("count must be >= 1 for key index " .. i)
    end

    -- Get or create epoch
    local current_epoch = redis.call("hget", meta_key, "e")
    if current_epoch == false then
        current_epoch = new_epoch
        redis.call("hset", meta_key, "e", current_epoch)
    end

    -- Increment offset by count (atomically reserves [old+1 .. old+count])
    local final_offset = redis.call("hincrby", meta_key, "s", count)

    table.insert(results, tostring(final_offset))
    table.insert(results, current_epoch)
end

return results
