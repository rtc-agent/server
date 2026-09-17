package centrifugeplus

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
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

//go:embed lua/publish_user_update.lua
var publishUserUpdateLua string

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
	publishUserUpdateScript *rueidis.Lua

	prefix string
	logger Logger
	tracer trace.Tracer

	// PUB/SUB support via DedicatedClient
	pubSubClient    rueidis.DedicatedClient
	pubSubCancel    func()
	pubSubMu        sync.Mutex
	subscribedChans map[string]bool

	// closed marks the broker as shut down. Once set, ensurePubSubClient
	// refuses to recreate the pubSubClient, preventing resource leaks
	// when Subscribe is called after Close.
	closed atomic.Bool
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
	publishUserUpdateScript := rueidis.NewLuaScript(publishUserUpdateLua)

	return &TopicBroker{
		config:                  config,
		redisClient:             redisClient,
		historyStore:            config.HistoryStore,
		incrbyOffsetScript:      incrbyOffsetScript,
		publishWithOffsetScript: publishWithOffsetScript,
		publishUserUpdateScript: publishUserUpdateScript,
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

// History returns publications for channel from HistoryStore.
// StreamPosition is read from Redis meta key.
func (b *TopicBroker) History(ch string, opts centrifuge.HistoryOptions) (pubs []*centrifuge.Publication, sp centrifuge.StreamPosition, err error) {
	// Interface methods cannot accept context; use an internal context with timeout to prevent Redis/DB queries from blocking indefinitely.
	// Recovery may involve many DB queries, so allow generous timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.history",
		trace.WithAttributes(AttributeChannel.String(ch)),
	)
	defer func() {
		if sp.Offset > 0 {
			span.SetAttributes(
				AttributeOffset.Int64(int64(sp.Offset)), //nolint:gosec // offset will not exceed int64 range
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
			sinceOffset = uint32(opts.Filter.Since.Offset) //nolint:gosec // range already checked
		}
	}

	// Get stream position from Redis meta key
	sp = b.getStreamPosition(ctx, ch)

	// Read from HistoryStore
	if b.historyStore == nil {
		return nil, sp, nil
	}

	pubs, err = b.historyStore.Query(ctx, ch, sinceOffset, uint32(sp.Offset)) //nolint:gosec // range already checked
	if err != nil {
		b.logger.Warn("HistoryStore.Query failed for channel %s: %v", ch, err)
		return nil, sp, err
	}

	return pubs, sp, nil
}

// getStreamPosition returns the current stream position for a channel.
// Reads offset from channel:offset:{ch} (same key as UpdatePublisher.save() writes to)
// and epoch from channel:epoch:{ch} (lazily initialized by PublishWithUserOffset).
// This ensures History returns the same offset sequence as real-time pushes.
func (b *TopicBroker) getStreamPosition(ctx context.Context, ch string) centrifuge.StreamPosition {
	offsetKey := b.channelOffsetKey(ch)
	epochKey := b.channelEpochKey(ch)

	// Use MGET to read both keys in a single round-trip
	result, err := b.redisClient.Do(ctx, b.redisClient.B().Mget().Key(offsetKey, epochKey).Build()).AsStrSlice()
	if err != nil {
		b.logger.Warn("getStreamPosition Redis query failed for channel %s: %v", ch, err)
		return centrifuge.StreamPosition{}
	}
	if len(result) < 2 {
		return centrifuge.StreamPosition{}
	}

	// Offset key may not exist for new channels
	if result[0] == "" {
		return centrifuge.StreamPosition{}
	}

	offset, err := strconv.ParseUint(result[0], 10, 64)
	if err != nil {
		b.logger.Warn("getStreamPosition: offset parse failed for channel %s: %v", ch, err)
		return centrifuge.StreamPosition{}
	}

	epoch := result[1] // May be empty if epoch not yet initialized (first publish hasn't happened)
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

	// Clean up meta key (legacy), offset key, and epoch key
	keysToDelete := []string{
		b.metaKey(ch),
		b.channelOffsetKey(ch),
		b.channelEpochKey(ch),
	}

	// Use WithTimeout to prevent indefinite blocking if Redis is unresponsive.
	delCtx, delCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer delCancel()
	if err := b.redisClient.Do(delCtx, b.redisClient.B().Del().Key(keysToDelete...).Build()).Error(); err != nil {
		errs = append(errs, fmt.Errorf("redis DEL: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("remove history errors: %v", errs)
	}
	return nil
}

// Close closes the broker and releases resources.
// After Close, ensurePubSubClient will refuse to recreate the pubSubClient,
// preventing DedicatedClient leaks from late Subscribe calls.
func (b *TopicBroker) Close(_ context.Context) error {
	b.pubSubMu.Lock()
	b.closed.Store(true) // Mark as closed before tearing down resources
	if b.pubSubCancel != nil {
		b.pubSubCancel()
	}
	b.pubSubClient = nil                      // Set to nil to prevent concurrent reuse.
	b.subscribedChans = make(map[string]bool) // Clear subscription state to ensure next Subscribe re-sends SUBSCRIBE.
	b.pubSubMu.Unlock()

	if b.redisClient != nil {
		b.redisClient.Close()
	}
	return nil
}

func (b *TopicBroker) metaKey(ch string) string {
	return b.prefix + ":meta:" + ch
}

// channelOffsetKey returns the Redis key for the channel offset counter.
// Consistent with cache.ChannelOffset(ch) ("channel:offset:" + ch).
// Note: centrifuge-plus is an independent module and cannot import internal/infra/cache, so the key format is hardcoded.
func (b *TopicBroker) channelOffsetKey(ch string) string {
	return "channel:offset:" + ch
}

// channelEpochKey returns the Redis key for the channel epoch.
// Consistent with cache.ChannelEpoch(ch) ("channel:epoch:" + ch).
func (b *TopicBroker) channelEpochKey(ch string) string {
	return "channel:epoch:" + ch
}

// generateEpoch generates a unique epoch string using UUID v7 (time-ordered).
func generateEpoch() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate epoch: %w", err)
	}
	return id.String(), nil
}

func (b *TopicBroker) mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		b.logger.Warn("failed to marshal: %v", err)
		return []byte("{}")
	}
	return data
}

// IncrConversationOffset allocates the next offset for a given session (Redis INCR).
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
	return uint32(result), nil //nolint:gosec // range already checked
}

// SetConversationOffset sets the offset value for a given session (Redis SET), used to initialize the counter in fork scenarios.
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
