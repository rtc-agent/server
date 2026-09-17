package centrifugeplus

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/centrifugal/centrifuge"
	"go.opentelemetry.io/otel/trace"
)

// BatchIncrby batch pre-allocates offsets for multiple channels.
// Call this before DB transaction. Returns map[channel]StreamPosition where the
// offset is the HIGHEST allocated offset for that channel; the full reserved
// range is [final - Count + 1, ..., final] (Count defaults to 1 if zero).
// Each channel must appear at most once in reqs.
func (b *TopicBroker) BatchIncrby(ctx context.Context, reqs []ChannelIncrbyRequest) (map[string]centrifuge.StreamPosition, error) {
	ctx, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.batch_incrby",
		trace.WithAttributes(AttributeChannel.String(fmt.Sprintf("%v", reqs))),
	)
	defer span.End()

	if len(reqs) == 0 {
		return make(map[string]centrifuge.StreamPosition), nil
	}

	// Normalize Count (0 → 1) and validate no duplicate channels.
	seen := make(map[string]struct{}, len(reqs))
	normalized := make([]ChannelIncrbyRequest, len(reqs))
	for i, req := range reqs {
		if req.Channel == "" {
			return nil, fmt.Errorf("channel is required at index %d", i)
		}
		if _, dup := seen[req.Channel]; dup {
			return nil, fmt.Errorf("duplicate channel %q in BatchIncrby request (use Count to allocate multiple offsets for one channel)", req.Channel)
		}
		seen[req.Channel] = struct{}{}
		count := req.Count
		if count <= 0 {
			count = 1
		}
		normalized[i] = ChannelIncrbyRequest{Channel: req.Channel, Count: count}
	}

	// Build KEYS and ARGV for Lua script.
	// ARGV layout: [count_1, epoch_1, count_2, epoch_2, ...]
	keys := make([]string, len(normalized))
	args := make([]string, len(normalized)*2)
	for i, req := range normalized {
		keys[i] = b.metaKey(req.Channel)
		args[i*2] = strconv.Itoa(req.Count)
		epoch, err := generateEpoch()
		if err != nil {
			return nil, fmt.Errorf("generate epoch for channel %s: %w", req.Channel, err)
		}
		args[i*2+1] = epoch
	}

	// Execute Lua script
	_, luaSpan := b.tracer.Start(ctx, "centrifugeplus.topicbroker.batch_incrby.lua")
	result, err := b.incrbyOffsetScript.Exec(ctx, b.redisClient, keys, args).AsStrSlice()
	if err != nil {
		recordError(luaSpan, err)
		luaSpan.End()
		return nil, fmt.Errorf("failed to execute incrby_offset script: %w", err)
	}
	luaSpan.End()

	if len(result) < len(normalized)*2 {
		return nil, fmt.Errorf("unexpected Lua script result length: %d, expected %d", len(result), len(normalized)*2)
	}

	// Parse results
	positions := make(map[string]centrifuge.StreamPosition, len(normalized))
	for i, req := range normalized {
		offset, err := strconv.ParseUint(result[i*2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse offset for channel %s: %w", req.Channel, err)
		}
		// Check if offset exceeds uint32 range, consistent with IncrConversationOffset.
		if offset > math.MaxUint32 {
			return nil, fmt.Errorf("offset overflow for channel %s: %d exceeds uint32 max", req.Channel, offset)
		}
		epoch := result[i*2+1]
		positions[req.Channel] = centrifuge.StreamPosition{Offset: offset, Epoch: epoch}
	}

	return positions, nil
}

// PublishWithOffset publishes a message using a pre-allocated offset.
// Call this after DB transaction commits. No HINCRBY, no XADD, no Asynq.
// Returns error if epoch mismatch occurs.
func (b *TopicBroker) PublishWithOffset(ctx context.Context, ch string, data []byte, opts centrifuge.PublishOptions, sp centrifuge.StreamPosition) error {
	ctx, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.publish_with_offset",
		trace.WithAttributes(
			AttributeChannel.String(ch),
			AttributeOffset.Int64(int64(sp.Offset)), //nolint:gosec // offset will not exceed int64 range
			AttributeEpoch.String(sp.Epoch),
		),
	)
	defer span.End()

	metaKey := b.metaKey(ch)
	resultKey := ""
	resultKeyExpire := ""

	// Build result key for idempotency
	if opts.IdempotencyKey != "" {
		resultKey = b.prefix + ":idempotent:" + ch + ":" + opts.IdempotencyKey
		ttl := int64(300)
		if opts.IdempotentResultTTL > 0 {
			ttl = int64(opts.IdempotentResultTTL.Seconds())
		}
		resultKeyExpire = strconv.FormatInt(ttl, 10)
	}

	payload := string(data)
	channel := b.pubSubKey(ch)
	publishCommand := "publish"
	traceparent := encodeTraceParent(span.SpanContext())

	keys := []string{metaKey, resultKey}
	args := []string{
		payload,
		channel,
		strconv.FormatUint(sp.Offset, 10),
		sp.Epoch,
		publishCommand,
		resultKeyExpire,
		traceparent,
	}

	// Execute Lua script
	_, luaSpan := b.tracer.Start(ctx, "centrifugeplus.topicbroker.publish_with_offset.lua")
	result, err := b.publishWithOffsetScript.Exec(ctx, b.redisClient, keys, args).AsStrSlice()
	if err != nil {
		recordError(luaSpan, err)
		luaSpan.End()
		return fmt.Errorf("failed to execute publish_with_offset script: %w", err)
	}
	luaSpan.End()

	if len(result) < 3 {
		return fmt.Errorf("unexpected Lua script result: %v", result)
	}

	// Check for epoch mismatch
	if result[0] == "-1" {
		currentEpoch := result[1]
		return fmt.Errorf("epoch mismatch: expected %s, got %s (channel %s)", sp.Epoch, currentEpoch, ch)
	}

	fromCache := result[2] == "1"
	if fromCache {
		span.SetAttributes(AttributeFromCache.Bool(true))
	}

	return nil
}

// PublishWithUserOffset publishes a user_update using the caller's pre-allocated offset.
// Unlike PublishWithContext (which calls BatchIncrby to allocate a separate stream offset),
// this method uses the user_update offset directly, ensuring consistency between
// the inner data.offset and the outer Publication.Offset.
//
// The epoch is lazily initialized via SETNX on first call for each channel,
// then read from Redis for subsequent calls. All within a single Lua script invocation.
func (b *TopicBroker) PublishWithUserOffset(ctx context.Context, ch string, data []byte, offset uint32, opts centrifuge.PublishOptions) (result centrifuge.PublishResult, err error) {
	ctx, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.publish_user_update",
		trace.WithAttributes(
			AttributeChannel.String(ch),
			AttributeOffset.Int64(int64(offset)),
		),
	)
	defer func() {
		span.SetAttributes(
			AttributeEpoch.String(result.Epoch),
			AttributeFromCache.Bool(result.Suppressed),
		)
		recordError(span, err)
		span.End()
	}()

	epochKey := b.channelEpochKey(ch)
	resultKey := ""
	resultKeyExpire := ""

	// Build result key for idempotency
	if opts.IdempotencyKey != "" {
		resultKey = b.prefix + ":idempotent:" + ch + ":" + opts.IdempotencyKey
		ttl := int64(300)
		if opts.IdempotentResultTTL > 0 {
			ttl = int64(opts.IdempotentResultTTL.Seconds())
		}
		resultKeyExpire = strconv.FormatInt(ttl, 10)
	}

	payload := string(data)
	pubSubChannel := b.pubSubKey(ch)
	publishCommand := "publish"
	traceparent := encodeTraceParent(span.SpanContext())
	defaultEpoch, err := generateEpoch()
	if err != nil {
		recordError(span, err)
		return centrifuge.PublishResult{}, fmt.Errorf("generate default epoch: %w", err)
	}

	keys := []string{epochKey, resultKey}
	args := []string{
		defaultEpoch,
		pubSubChannel,
		strconv.FormatUint(uint64(offset), 10),
		payload,
		publishCommand,
		resultKeyExpire,
		traceparent,
	}

	// Execute Lua script
	_, luaSpan := b.tracer.Start(ctx, "centrifugeplus.topicbroker.publish_user_update.lua")
	scriptResult, scriptErr := b.publishUserUpdateScript.Exec(ctx, b.redisClient, keys, args).AsStrSlice()
	if scriptErr != nil {
		recordError(luaSpan, scriptErr)
		luaSpan.End()
		return centrifuge.PublishResult{}, fmt.Errorf("failed to execute publish_user_update script: %w", scriptErr)
	}
	luaSpan.End()

	if len(scriptResult) < 3 {
		return centrifuge.PublishResult{}, fmt.Errorf("unexpected publish_user_update script result: %v", scriptResult)
	}

	resultOffset, parseErr := strconv.ParseUint(scriptResult[0], 10, 64)
	if parseErr != nil {
		return centrifuge.PublishResult{}, fmt.Errorf("parse offset from script result: %w", parseErr)
	}
	resultEpoch := scriptResult[1]
	fromCache := scriptResult[2] == "1"
	if fromCache {
		span.SetAttributes(AttributeFromCache.Bool(true))
	}

	return centrifuge.PublishResult{
		StreamPosition: centrifuge.StreamPosition{Offset: resultOffset, Epoch: resultEpoch},
		Suppressed:     fromCache,
	}, nil
}

// Publish is a convenience method that internally calls BatchIncrby + PublishWithOffset.
// For IM scenarios, use BatchIncrby → DB transaction → PublishWithOffset instead.
// Uses WithTimeout to prevent indefinite blocking if Redis is unresponsive.
// The 5s timeout matches PublishJoin/PublishLeave in topic_broker.go.
func (b *TopicBroker) Publish(ch string, data []byte, opts centrifuge.PublishOptions) (centrifuge.PublishResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return b.PublishWithContext(ctx, ch, data, opts)
}

// PublishWithContext is like Publish but accepts a context for distributed tracing.
func (b *TopicBroker) PublishWithContext(ctx context.Context, ch string, data []byte, opts centrifuge.PublishOptions) (result centrifuge.PublishResult, err error) {
	ctx, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.publish",
		trace.WithAttributes(AttributeChannel.String(ch)),
	)
	defer func() {
		span.SetAttributes(
			AttributeOffset.Int64(int64(result.Offset)), //nolint:gosec // offset will not exceed int64 range
			AttributeEpoch.String(result.Epoch),
			AttributeFromCache.Bool(result.Suppressed), // reuse Suppressed field to indicate fromCache
		)
		recordError(span, err)
		span.End()
	}()

	// Check idempotency cache first (before BatchIncrby increments offset)
	if opts.IdempotencyKey != "" {
		resultKey := b.prefix + ":idempotent:" + ch + ":" + opts.IdempotencyKey
		cached, err := b.redisClient.Do(ctx, b.redisClient.B().Hmget().Key(resultKey).Field("e", "s").Build()).AsStrSlice()
		if err == nil && len(cached) >= 2 && cached[0] != "" {
			offset, parseErr := strconv.ParseUint(cached[1], 10, 64)
			if parseErr == nil {
				return centrifuge.PublishResult{
					StreamPosition: centrifuge.StreamPosition{Offset: offset, Epoch: cached[0]},
					Suppressed:     true,
					SuppressReason: centrifuge.SuppressReasonIdempotency,
				}, nil
			}
		}
	}

	// Step 1: BatchIncrby
	positions, err := b.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: ch}})
	if err != nil {
		return centrifuge.PublishResult{}, fmt.Errorf("BatchIncrby failed: %w", err)
	}

	position := positions[ch]

	// Step 2: PublishWithOffset
	if err := b.PublishWithOffset(ctx, ch, data, opts, position); err != nil {
		return centrifuge.PublishResult{}, fmt.Errorf("PublishWithOffset failed: %w", err)
	}

	return centrifuge.PublishResult{StreamPosition: position}, nil
}

// PublishJoin publishes Join message to channel.
func (b *TopicBroker) PublishJoin(ch string, info *centrifuge.ClientInfo) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.publish_join",
		trace.WithAttributes(AttributeChannel.String(ch)),
	)
	defer span.End()

	pubSubKey := b.pubSubKey(ch)
	payload := "__" + "j1:" + string(b.mustMarshal(info))
	span.SetAttributes(AttributeMessageType.String("join"))

	err := b.redisClient.Do(ctx, b.redisClient.B().Publish().Channel(pubSubKey).Message(payload).Build()).Error()
	recordError(span, err)
	return err
}

// PublishLeave publishes Leave message to channel.
func (b *TopicBroker) PublishLeave(ch string, info *centrifuge.ClientInfo) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.publish_leave",
		trace.WithAttributes(AttributeChannel.String(ch)),
	)
	defer span.End()

	pubSubKey := b.pubSubKey(ch)
	payload := "__" + "l1:" + string(b.mustMarshal(info))
	span.SetAttributes(AttributeMessageType.String("leave"))

	err := b.redisClient.Do(ctx, b.redisClient.B().Publish().Channel(pubSubKey).Message(payload).Build()).Error()
	recordError(span, err)
	return err
}
