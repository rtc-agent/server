package centrifugeplus

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
	"github.com/redis/rueidis"
	"go.opentelemetry.io/otel/trace"
)

//go:embed lua/incrby_offset.lua
var incrbyOffsetLua string

//go:embed lua/publish_with_offset.lua
var publishWithOffsetLua string

// ChannelIncrbyRequest represents a channel to pre-allocate offsets for.
// Count specifies how many consecutive offsets to reserve (default 1 if zero).
// The returned StreamPosition for the channel carries the HIGHEST allocated offset;
// callers derive the full range as [final - Count + 1, ..., final].
// Each channel must appear at most once per BatchIncrby call.
type ChannelIncrbyRequest struct {
	Channel string
	Count   int
}

// TopicBroker implements centrifuge.Broker interface for Topic mode channels.
// It uses a "persist first, then push" model: BatchIncrby → DB transaction → PublishWithOffset.
type TopicBroker struct {
	eventHandler atomic.Pointer[centrifuge.BrokerEventHandler]
	config       TopicBrokerConfig
	redisClient  rueidis.Client
	historyStore HistoryStore

	incrbyOffsetScript      *rueidis.Lua
	publishWithOffsetScript *rueidis.Lua

	prefix string
	logger Logger
	tracer trace.Tracer

	// PUB/SUB support via DedicatedClient
	pubSubClient    rueidis.DedicatedClient
	pubSubCancel    func()
	pubSubMu        sync.Mutex
	subscribedChans map[string]bool
}

// NewTopicBroker creates a new TopicBroker instance.
func NewTopicBroker(config TopicBrokerConfig) (*TopicBroker, error) {
	if config.RedisAddr == "" {
		return nil, fmt.Errorf("redis address is required")
	}
	if config.Prefix == "" {
		config.Prefix = "centrifuge"
	}

	prefix := config.Prefix

	if config.Logger == nil {
		config.Logger = defaultLogger{}
	}

	// Create rueidis client
	redisClient, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{config.RedisAddr},
		Password:    config.RedisPassword,
		SelectDB:    config.RedisDB,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create redis client: %w", err)
	}

	incrbyOffsetScript := rueidis.NewLuaScript(incrbyOffsetLua)
	publishWithOffsetScript := rueidis.NewLuaScript(publishWithOffsetLua)

	return &TopicBroker{
		config:                  config,
		redisClient:             redisClient,
		historyStore:            config.HistoryStore,
		incrbyOffsetScript:      incrbyOffsetScript,
		publishWithOffsetScript: publishWithOffsetScript,
		prefix:                  prefix,
		logger:                  config.Logger,
		tracer:                  config.Tracing.tracer(),
		subscribedChans:         make(map[string]bool),
	}, nil
}

// RegisterBrokerEventHandler is called once when Broker is set to Node.
func (b *TopicBroker) RegisterBrokerEventHandler(handler centrifuge.BrokerEventHandler) error {
	b.eventHandler.Store(&handler)
	return nil
}

// ensurePubSubClient creates a DedicatedClient for PUB/SUB if not already created.
// Must be called with pubSubMu held.
func (b *TopicBroker) ensurePubSubClient() error {
	if b.pubSubClient != nil {
		return nil
	}

	client, cancel := b.redisClient.Dedicate()
	b.pubSubClient = client
	b.pubSubCancel = cancel

	// Set up message handler
	client.SetPubSubHooks(rueidis.PubSubHooks{
		OnMessage: func(msg rueidis.PubSubMessage) {
			b.handlePubSubMessage(msg)
		},
	})

	return nil
}

// handlePubSubMessage processes incoming PUB/SUB messages and forwards them to centrifuge.
func (b *TopicBroker) handlePubSubMessage(msg rueidis.PubSubMessage) {
	// 原子快照：加载一次，整个函数使用同一个 handler，避免并发 Store 导致的不一致
	h := b.eventHandler.Load()
	if h == nil {
		return
	}

	rawPayload := msg.Message
	ch := msg.Channel
	prefix := b.prefix + ":pubsub:"
	if trimmed, ok := strings.CutPrefix(ch, prefix); ok {
		ch = trimmed
	} else {
		return
	}

	// 根据消息前缀判断类型，只对 publication 消息提取 trace parent
	if strings.HasPrefix(rawPayload, "__p1:") {
		// Publication 消息：提取 trace parent 并创建 span
		payload, traceparentStr := extractTraceParentFromPayload(rawPayload)

		var sc trace.SpanContext
		if traceparentStr != "" {
			if parsed, err := decodeTraceParent(traceparentStr); err == nil {
				sc = parsed
			}
		}

		// Create span with remote parent if trace context was propagated.
		spanCtx := context.Background()
		if sc.IsValid() {
			spanCtx = trace.ContextWithRemoteSpanContext(spanCtx, sc)
		}
		_, span := b.tracer.Start(spanCtx, "centrifugeplus.topicbroker.pubsub",
			trace.WithAttributes(
				AttributeChannel.String(ch),
				AttributeMessageType.String("publication"),
			),
		)
		defer span.End()

		// Format: __p1:{offset}:{epoch}:{data_len}__{data}
		// Use length-prefix to safely separate meta from data (avoids __ conflicts in message content).
		raw := payload[5:] // skip "__p1:"
		meta, encodedData, ok := strings.Cut(raw, "__")
		if !ok {
			b.logger.Warn("pubsub message missing meta/data separator in channel %s", ch)
			return
		}

		parts := strings.SplitN(meta, ":", 3)
		if len(parts) != 3 {
			b.logger.Warn("pubsub message invalid meta format in channel %s: %q", ch, meta)
			return
		}
		offset, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			b.logger.Warn("pubsub message invalid offset in channel %s: %v", ch, err)
			return
		}
		epoch := parts[1]
		dataLen, err := strconv.Atoi(parts[2])
		if err != nil || dataLen < 0 || dataLen > len(encodedData) {
			b.logger.Warn("pubsub message invalid data_len in channel %s: err=%v, dataLen=%d, available=%d", ch, err, dataLen, len(encodedData))
			return
		}
		data := encodedData[:dataLen]

		span.SetAttributes(
			AttributeOffset.Int64(int64(offset)), //nolint:gosec // offset 不会超过 int64 范围
			AttributeEpoch.String(epoch),
		)

		pub := &centrifuge.Publication{
			Data:   []byte(data),
			Offset: offset,
		}
		sp := centrifuge.StreamPosition{
			Offset: offset,
			Epoch:  epoch,
		}
		if err := (*h).HandlePublication(ch, pub, sp, false, nil); err != nil {
			b.logger.Warn("failed to handle publication for channel %s: %v", ch, err)
			recordError(span, err)
		}
		return
	}

	// Join/Leave 消息：无需提取 trace parent，直接处理
	if strings.HasPrefix(rawPayload, "__j1:") {
		_, span := b.tracer.Start(context.Background(), "centrifugeplus.topicbroker.pubsub",
			trace.WithAttributes(
				AttributeChannel.String(ch),
				AttributeMessageType.String("join"),
			),
		)
		defer span.End()

		var info centrifuge.ClientInfo
		if err := json.Unmarshal([]byte(rawPayload[5:]), &info); err != nil {
			b.logger.Warn("failed to unmarshal join info: %v", err)
			recordError(span, err)
			return
		}
		if err := (*h).HandleJoin(ch, &info); err != nil {
			b.logger.Warn("failed to handle join for channel %s: %v", ch, err)
			recordError(span, err)
		}
		return
	}

	if strings.HasPrefix(rawPayload, "__l1:") {
		_, span := b.tracer.Start(context.Background(), "centrifugeplus.topicbroker.pubsub",
			trace.WithAttributes(
				AttributeChannel.String(ch),
				AttributeMessageType.String("leave"),
			),
		)
		defer span.End()

		var info centrifuge.ClientInfo
		if err := json.Unmarshal([]byte(rawPayload[5:]), &info); err != nil {
			b.logger.Warn("failed to unmarshal leave info: %v", err)
			recordError(span, err)
			return
		}
		if err := (*h).HandleLeave(ch, &info); err != nil {
			b.logger.Warn("failed to handle leave for channel %s: %v", ch, err)
			recordError(span, err)
		}
		return
	}
}

// Subscribe subscribes node to channels.
func (b *TopicBroker) Subscribe(channels ...string) error {
	for _, ch := range channels {
		// 接口方法无法接受 context，内部使用带超时的 context 防止 Redis 命令无限阻塞
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		_, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.subscribe",
			trace.WithAttributes(AttributeChannel.String(ch)),
		)

		b.pubSubMu.Lock()

		if err := b.ensurePubSubClient(); err != nil {
			b.pubSubMu.Unlock()
			cancel()
			recordError(span, err)
			span.End()
			return fmt.Errorf("failed to create pubsub client: %w", err)
		}

		pubSubKey := b.pubSubKey(ch)
		if b.subscribedChans[pubSubKey] {
			b.pubSubMu.Unlock()
			cancel()
			span.End()
			continue // Already subscribed
		}

		if err := b.pubSubClient.Do(ctx, b.pubSubClient.B().Subscribe().Channel(pubSubKey).Build()).Error(); err != nil {
			b.pubSubMu.Unlock()
			cancel()
			recordError(span, err)
			span.End()
			return fmt.Errorf("failed to subscribe to %s: %w", pubSubKey, err)
		}

		b.subscribedChans[pubSubKey] = true
		b.pubSubMu.Unlock()
		cancel()
		span.End()
	}
	return nil
}

// Unsubscribe unsubscribes node from channels.
func (b *TopicBroker) Unsubscribe(channels ...string) error {
	for _, ch := range channels {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		_, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.unsubscribe",
			trace.WithAttributes(AttributeChannel.String(ch)),
		)

		b.pubSubMu.Lock()

		if b.pubSubClient == nil {
			b.pubSubMu.Unlock()
			cancel()
			span.End()
			continue
		}

		pubSubKey := b.pubSubKey(ch)
		if !b.subscribedChans[pubSubKey] {
			b.pubSubMu.Unlock()
			cancel()
			span.End()
			continue // Not subscribed
		}

		if err := b.pubSubClient.Do(ctx, b.pubSubClient.B().Unsubscribe().Channel(pubSubKey).Build()).Error(); err != nil {
			b.pubSubMu.Unlock()
			cancel()
			recordError(span, err)
			span.End()
			return fmt.Errorf("failed to unsubscribe from %s: %w", pubSubKey, err)
		}

		delete(b.subscribedChans, pubSubKey)
		b.pubSubMu.Unlock()
		cancel()
		span.End()
	}
	return nil
}

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
		args[i*2+1] = generateEpoch()
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
		// 检查 offset 是否超出 uint32 范围，与 IncrConversationOffset 保持一致
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
			AttributeOffset.Int64(int64(sp.Offset)), //nolint:gosec // offset 不会超过 int64 范围
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

// Publish is a convenience method that internally calls BatchIncrby + PublishWithOffset.
// For IM scenarios, use BatchIncrby → DB transaction → PublishWithOffset instead.
func (b *TopicBroker) Publish(ch string, data []byte, opts centrifuge.PublishOptions) (centrifuge.PublishResult, error) {
	return b.PublishWithContext(context.Background(), ch, data, opts)
}

// PublishWithContext is like Publish but accepts a context for distributed tracing.
func (b *TopicBroker) PublishWithContext(ctx context.Context, ch string, data []byte, opts centrifuge.PublishOptions) (result centrifuge.PublishResult, err error) {
	ctx, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.publish",
		trace.WithAttributes(AttributeChannel.String(ch)),
	)
	defer func() {
		span.SetAttributes(
			AttributeOffset.Int64(int64(result.Offset)), //nolint:gosec // offset 不会超过 int64 范围
			AttributeEpoch.String(result.Epoch),
			AttributeFromCache.Bool(result.Suppressed), // 复用 Suppressed 字段表示 fromCache
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

// History returns publications for channel from HistoryStore.
// StreamPosition is read from Redis meta key.
func (b *TopicBroker) History(ch string, opts centrifuge.HistoryOptions) (pubs []*centrifuge.Publication, sp centrifuge.StreamPosition, err error) {
	// 接口方法无法接受 context，内部使用带超时的 context 防止 Redis/DB 查询无限阻塞
	// Recovery 可能涉及大量 DB 查询，给予充足的超时时间
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.history",
		trace.WithAttributes(AttributeChannel.String(ch)),
	)
	defer func() {
		if sp.Offset > 0 {
			span.SetAttributes(
				AttributeOffset.Int64(int64(sp.Offset)), //nolint:gosec // offset 不会超过 int64 范围
				AttributeEpoch.String(sp.Epoch),
			)
		}
		recordError(span, err)
		span.End()
	}()

	sinceOffset := uint32(0)
	if opts.Filter.Since != nil {
		if opts.Filter.Since.Offset > math.MaxUint32 {
			b.logger.Warn("History sinceOffset %d exceeds uint32 range, clamping to MaxUint32", opts.Filter.Since.Offset)
			sinceOffset = math.MaxUint32
		} else {
			sinceOffset = uint32(opts.Filter.Since.Offset) //nolint:gosec // 已检查范围
		}
	}

	// Get stream position from Redis meta key
	sp = b.getStreamPosition(ctx, ch)

	// Read from HistoryStore
	if b.historyStore == nil {
		return nil, sp, nil
	}

	pubs, err = b.historyStore.Query(ctx, ch, sinceOffset, uint32(sp.Offset)) //nolint:gosec // offset 不会超过 uint32 范围
	if err != nil {
		b.logger.Warn("HistoryStore.Query failed for channel %s: %v", ch, err)
		return nil, sp, err
	}

	return pubs, sp, nil
}

// getStreamPosition returns the current stream position for a channel from Redis meta key.
func (b *TopicBroker) getStreamPosition(ctx context.Context, ch string) centrifuge.StreamPosition {
	metaKey := b.metaKey(ch)
	result, err := b.redisClient.Do(ctx, b.redisClient.B().Hmget().Key(metaKey).Field("s", "e").Build()).AsStrSlice()
	if err != nil {
		b.logger.Warn("getStreamPosition Redis query failed for channel %s: %v", ch, err)
		return centrifuge.StreamPosition{}
	}
	if len(result) < 2 {
		return centrifuge.StreamPosition{}
	}
	// 新频道 meta key 不存在时，Redis 返回空字符串，直接返回零值
	if result[0] == "" {
		return centrifuge.StreamPosition{}
	}
	offset, err := strconv.ParseUint(result[0], 10, 64)
	if err != nil {
		b.logger.Warn("getStreamPosition: offset parse failed for channel %s: %v", ch, err)
		return centrifuge.StreamPosition{}
	}
	epoch := result[1]
	return centrifuge.StreamPosition{Offset: offset, Epoch: epoch}
}

// RemoveHistory removes history from channel.
func (b *TopicBroker) RemoveHistory(ch string) error {
	var errs []error

	if remover, ok := b.historyStore.(HistoryStoreRemover); ok {
		if err := remover.RemoveHistory(ch); err != nil {
			errs = append(errs, fmt.Errorf("history store: %w", err))
		}
	}

	metaKey := b.metaKey(ch)

	if err := b.redisClient.Do(context.Background(), b.redisClient.B().Del().Key(metaKey).Build()).Error(); err != nil {
		errs = append(errs, fmt.Errorf("redis DEL: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("remove history errors: %v", errs)
	}
	return nil
}

// Close closes the broker and releases resources.
func (b *TopicBroker) Close(_ context.Context) error {
	b.pubSubMu.Lock()
	if b.pubSubCancel != nil {
		b.pubSubCancel()
	}
	b.pubSubClient = nil                      // 置空防止并发复用
	b.subscribedChans = make(map[string]bool) // 清空订阅状态，确保下次 Subscribe 重新发送 SUBSCRIBE
	b.pubSubMu.Unlock()

	if b.redisClient != nil {
		b.redisClient.Close()
	}
	return nil
}

func (b *TopicBroker) pubSubKey(ch string) string {
	return b.prefix + ":pubsub:" + ch
}

func (b *TopicBroker) metaKey(ch string) string {
	return b.prefix + ":meta:" + ch
}

// generateEpoch generates a unique epoch string using UUID v7 (time-ordered).
func generateEpoch() string {
	id, err := uuid.NewV7()
	if err != nil {
		panic(err) // 仅当系统随机源耗尽时发生
	}
	return id.String()
}

func (b *TopicBroker) mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		b.logger.Warn("failed to marshal: %v", err)
		return []byte("{}")
	}
	return data
}

// IncrConversationOffset 为指定会话分配下一个 offset（Redis INCR）
// key: {prefix}:conv:offset:{conversationID}
func (b *TopicBroker) IncrConversationOffset(ctx context.Context, conversationID string) (uint32, error) {
	key := b.prefix + ":conv:offset:" + conversationID
	cmd := b.redisClient.B().Incr().Key(key).Build()
	result, err := b.redisClient.Do(ctx, cmd).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("incr conversation offset: %w", err)
	}
	if result > int64(math.MaxUint32) {
		return 0, fmt.Errorf("conversation offset overflow: %d", result)
	}
	return uint32(result), nil //nolint:gosec // 已检查范围
}

// SetConversationOffset 设置指定会话的 offset 值（Redis SET），用于 fork 等场景初始化计数器
func (b *TopicBroker) SetConversationOffset(ctx context.Context, conversationID string, value uint32) error {
	key := b.prefix + ":conv:offset:" + conversationID
	cmd := b.redisClient.B().Set().Key(key).Value(strconv.FormatUint(uint64(value), 10)).Build()
	if err := b.redisClient.Do(ctx, cmd).Error(); err != nil {
		return fmt.Errorf("set conversation offset: %w", err)
	}
	return nil
}

// Ensure TopicBroker implements centrifuge.Broker
var _ centrifuge.Broker = (*TopicBroker)(nil)

// Ensure TopicBroker implements centrifuge.Closer
var _ centrifuge.Closer = (*TopicBroker)(nil)
