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
	trimmed, ok := strings.CutPrefix(ch, prefix)
	if !ok {
		return
	}
	ch = trimmed

	// Dispatch by message type prefix; only publication messages need trace extraction.
	switch {
	case strings.HasPrefix(rawPayload, "__p1:"):
		b.handlePublicationMessage(h, ch, rawPayload)
	case strings.HasPrefix(rawPayload, "__j1:"):
		b.handleJoinMessage(h, ch, rawPayload)
	case strings.HasPrefix(rawPayload, "__l1:"):
		b.handleLeaveMessage(h, ch, rawPayload)
	}
}

// handlePublicationMessage processes a publication ("__p1:") PUB/SUB message.
// Format: __p1:{offset}:{epoch}:{data_len}__{data}
// A "__p1:" traceparent prefix may precede the payload for distributed tracing.
func (b *TopicBroker) handlePublicationMessage(h *centrifuge.BrokerEventHandler, ch, rawPayload string) {
	// Extract trace parent (if propagated) and create span.
	payload, traceparentStr := extractTraceParentFromPayload(rawPayload)

	var sc trace.SpanContext
	if traceparentStr != "" {
		if parsed, err := decodeTraceParent(traceparentStr); err == nil {
			sc = parsed
		}
	}

	spanCtx := context.Background()
	if sc.IsValid() {
		spanCtx = trace.ContextWithRemoteSpanContext(spanCtx, sc)
	}
	_, span := b.tracer.Start(spanCtx, "centrifuge_plus.topic_broker.pubsub",
		trace.WithAttributes(
			AttributeChannel.String(ch),
			AttributeMessageType.String("publication"),
		),
	)
	defer span.End()

	// Use length-prefix to safely separate meta from data (avoids __ conflicts in message content).
	raw := payload[5:] // skip "__p1:"
	meta, encodedData, ok := strings.Cut(raw, "__")
	if !ok {
		b.logger.Warn("pubsub message missing meta/data separator in channel %s", ch)
		return
	}

	offset, epoch, dataLen, err := parsePublicationMeta(meta, len(encodedData), ch)
	if err != nil {
		b.logger.Warn("pubsub message invalid meta in channel %s: %v", ch, err)
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
}

// parsePublicationMeta parses the "{offset}:{epoch}:{data_len}" meta portion of
// a publication message, returning the parsed values or an error describing the issue.
func parsePublicationMeta(meta string, availableBytes int, ch string) (uint64, string, int, error) {
	parts := strings.SplitN(meta, ":", 3)
	if len(parts) != 3 {
		return 0, "", 0, fmt.Errorf("invalid meta format: %q", meta)
	}
	offset, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, "", 0, fmt.Errorf("invalid offset: %w", err)
	}
	dataLen, err := strconv.Atoi(parts[2])
	if err != nil || dataLen < 0 || dataLen > availableBytes {
		return 0, "", 0, fmt.Errorf("invalid data_len: err=%v, dataLen=%d, available=%d", err, dataLen, availableBytes)
	}
	return offset, parts[1], dataLen, nil
}

// handleJoinMessage processes a join ("__j1:") PUB/SUB message.
func (b *TopicBroker) handleJoinMessage(h *centrifuge.BrokerEventHandler, ch, rawPayload string) {
	_, span := b.tracer.Start(context.Background(), "centrifuge_plus.topic_broker.pubsub",
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
}

// handleLeaveMessage processes a leave ("__l1:") PUB/SUB message.
func (b *TopicBroker) handleLeaveMessage(h *centrifuge.BrokerEventHandler, ch, rawPayload string) {
	_, span := b.tracer.Start(context.Background(), "centrifuge_plus.topic_broker.pubsub",
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
}

// Subscribe subscribes node to channels.
func (b *TopicBroker) Subscribe(channels ...string) error {
	for _, ch := range channels {
		// Interface methods cannot accept context; use an internal context with timeout to prevent Redis commands from blocking indefinitely.
		// NOTE: cancel() is called explicitly at the end of each iteration to prevent resource leaks
		// when subscribing to many channels (defer would accumulate until function returns).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		_, span := b.tracer.Start(ctx, "centrifuge_plus.topic_broker.subscribe",
			trace.WithAttributes(AttributeChannel.String(ch)),
		)

		// Only check and save reference inside the lock; execute Redis network operations after releasing the lock.
		b.pubSubMu.Lock()
		if err := b.ensurePubSubClient(); err != nil {
			b.pubSubMu.Unlock()
			recordError(span, err)
			span.End()
			cancel()
			return fmt.Errorf("failed to create pubsub client: %w", err)
		}

		pubSubKey := b.pubSubKey(ch)
		if b.subscribedChans[pubSubKey] {
			b.pubSubMu.Unlock()
			span.End()
			cancel()
			continue // Already subscribed
		}
		client := b.pubSubClient // Save reference for use outside the lock.
		b.pubSubMu.Unlock()

		// Redis network operations execute outside the lock to avoid blocking other Subscribe/Unsubscribe.
		if err := client.Do(ctx, client.B().Subscribe().Channel(pubSubKey).Build()).Error(); err != nil {
			recordError(span, err)
			span.End()
			cancel()
			return fmt.Errorf("failed to subscribe to %s: %w", pubSubKey, err)
		}

		// Re-acquire lock to update state.
		b.pubSubMu.Lock()
		b.subscribedChans[pubSubKey] = true
		b.pubSubMu.Unlock()
		span.End()
		cancel()
	}
	return nil
}

// Unsubscribe unsubscribes node from channels.
func (b *TopicBroker) Unsubscribe(channels ...string) error {
	for _, ch := range channels {
		// NOTE: cancel() is called explicitly at the end of each iteration to prevent resource leaks
		// when unsubscribing from many channels (defer would accumulate until function returns).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		_, span := b.tracer.Start(ctx, "centrifuge_plus.topic_broker.unsubscribe",
			trace.WithAttributes(AttributeChannel.String(ch)),
		)

		b.pubSubMu.Lock()

		if b.pubSubClient == nil {
			b.pubSubMu.Unlock()
			span.End()
			cancel()
			continue
		}

		pubSubKey := b.pubSubKey(ch)
		if !b.subscribedChans[pubSubKey] {
			b.pubSubMu.Unlock()
			span.End()
			cancel()
			continue // Not subscribed
		}
		client := b.pubSubClient // Save reference for use outside the lock.
		delete(b.subscribedChans, pubSubKey)
		b.pubSubMu.Unlock()

		// Redis network operations execute outside the lock to avoid blocking other Subscribe/Unsubscribe.
		if err := client.Do(ctx, client.B().Unsubscribe().Channel(pubSubKey).Build()).Error(); err != nil {
			recordError(span, err)
			span.End()
			cancel()
			return fmt.Errorf("failed to unsubscribe from %s: %w", pubSubKey, err)
		}

		span.End()
		cancel()
	}
	return nil
}

// pubSubKey returns the Redis PUB/SUB channel key for a given channel name.
func (b *TopicBroker) pubSubKey(ch string) string {
	return b.prefix + ":pubsub:" + ch
}
