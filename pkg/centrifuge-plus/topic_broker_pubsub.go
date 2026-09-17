package centrifugeplus

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/redis/rueidis"
	"go.opentelemetry.io/otel/trace"
)

// ensurePubSubClient creates a DedicatedClient for PUB/SUB if not already created.
// Must be called with pubSubMu held.
// Returns an error if the broker has been closed, preventing resource leaks
// from recreating clients after shutdown.
func (b *TopicBroker) ensurePubSubClient() error {
	if b.closed.Load() {
		return fmt.Errorf("topic broker is closed")
	}
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
	// Atomic snapshot: load once, use the same handler throughout the function, avoiding inconsistency from concurrent Store.
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

	// Determine message type by prefix; only extract trace parent for publication messages.
	if strings.HasPrefix(rawPayload, "__p1:") {
		// Publication message: extract trace parent and create span.
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
			AttributeOffset.Int64(int64(offset)), //nolint:gosec // offset will not exceed int64 range
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

	// Join/Leave messages: no need to extract trace parent, process directly.
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
		// Interface methods cannot accept context; use an internal context with timeout to prevent Redis commands from blocking indefinitely.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		_, span := b.tracer.Start(ctx, "centrifugeplus.topicbroker.subscribe",
			trace.WithAttributes(AttributeChannel.String(ch)),
		)

		// Only check and save reference inside the lock; execute Redis network operations after releasing the lock.
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
		client := b.pubSubClient // Save reference for use outside the lock.
		b.pubSubMu.Unlock()

		// Redis network operations execute outside the lock to avoid blocking other Subscribe/Unsubscribe.
		if err := client.Do(ctx, client.B().Subscribe().Channel(pubSubKey).Build()).Error(); err != nil {
			cancel()
			recordError(span, err)
			span.End()
			return fmt.Errorf("failed to subscribe to %s: %w", pubSubKey, err)
		}

		// Re-acquire lock to update state.
		b.pubSubMu.Lock()
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
		client := b.pubSubClient // Save reference for use outside the lock.
		delete(b.subscribedChans, pubSubKey)
		b.pubSubMu.Unlock()

		// Redis network operations execute outside the lock to avoid blocking other Subscribe/Unsubscribe.
		if err := client.Do(ctx, client.B().Unsubscribe().Channel(pubSubKey).Build()).Error(); err != nil {
			cancel()
			recordError(span, err)
			span.End()
			return fmt.Errorf("failed to unsubscribe from %s: %w", pubSubKey, err)
		}

		cancel()
		span.End()
	}
	return nil
}

// pubSubKey returns the Redis PUB/SUB channel key for a given channel name.
func (b *TopicBroker) pubSubKey(ch string) string {
	return b.prefix + ":pubsub:" + ch
}
