package centrifugeplus

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/centrifugal/centrifuge"
	"go.opentelemetry.io/otel/trace"
)

// DualBroker implements centrifuge.Broker interface.
// It routes messages to either RedisBroker (Live) or TopicBroker (Topic) based on channel type.
type DualBroker struct {
	liveBroker  *centrifuge.RedisBroker
	topicBroker *TopicBroker
	// channelTypes maps channel name to its type. Entries are added on Subscribe
	// (including prefix-based inference) and removed on Unsubscribe.
	// Cardinality is bounded by the number of active channels (e.g. active users),
	// which is moderate. Unsubscribe cleans up entries; Close clears all remaining.
	channelTypes sync.Map // map[string]ChannelType
	tracer       trace.Tracer
}

// NewDualBroker creates a new DualBroker instance.
// The node parameter is required for RedisBroker initialization.
func NewDualBroker(node *centrifuge.Node, config DualBrokerConfig) (*DualBroker, error) {
	liveBroker, err := centrifuge.NewRedisBroker(node, config.Live)
	if err != nil {
		return nil, err
	}

	topicBroker, err := NewTopicBroker(config.Topic)
	if err != nil {
		return nil, err
	}

	return &DualBroker{
		liveBroker:  liveBroker,
		topicBroker: topicBroker,
		tracer:      config.Topic.Tracing.tracer(),
	}, nil
}

// RegisterChannelType registers the channel type for a given channel.
func (d *DualBroker) RegisterChannelType(ch string, ct ChannelType) {
	d.channelTypes.Store(ch, ct)
}

// TopicBroker returns the underlying TopicBroker used for Topic mode channels.
func (d *DualBroker) TopicBroker() *TopicBroker {
	return d.topicBroker
}

// getChannelType returns the channel type for a given channel.
// Returns an error instead of a default value to prevent unregistered channels from being silently routed to Live, causing message loss.
// When the channel type is not explicitly registered, infers and registers it based on channel name prefix (handles publish-before-subscribe scenarios).
func (d *DualBroker) getChannelType(ch string) (ChannelType, error) {
	if v, ok := d.channelTypes.Load(ch); ok {
		return v.(ChannelType), nil
	}
	// Fallback: infer channel type by prefix (topic: -> Topic, live: -> Live).
	switch {
	case len(ch) > 6 && ch[:6] == "topic:":
		d.channelTypes.Store(ch, Topic)
		return Topic, nil
	case len(ch) > 5 && ch[:5] == "live:":
		d.channelTypes.Store(ch, Live)
		return Live, nil
	}
	return ChannelType(0), fmt.Errorf("channel type not registered for %q", ch)
}

// RegisterBrokerEventHandler is called once when Broker is set to Node.
func (d *DualBroker) RegisterBrokerEventHandler(handler centrifuge.BrokerEventHandler) error {
	if err := d.liveBroker.RegisterBrokerEventHandler(handler); err != nil {
		return err
	}
	return d.topicBroker.RegisterBrokerEventHandler(handler)
}

// Subscribe subscribes node to channels.
// Note: the interface signature does not include a context parameter (consistent with centrifuge.NodeBroker);
// context.Background() here is only used for tracing span creation, not affecting business logic.
func (d *DualBroker) Subscribe(channels ...string) error {
	for _, ch := range channels {
		ct, err := d.getChannelType(ch)
		if err != nil {
			return err
		}
		_, span := d.tracer.Start(context.Background(), "centrifugeplus.dualbroker.subscribe",
			trace.WithAttributes(
				AttributeChannel.String(ch),
				AttributeChannelType.String(ct.String()),
			),
		)

		var subscribeErr error
		switch ct {
		case Topic:
			subscribeErr = d.topicBroker.Subscribe(ch)
		default: // Live
			subscribeErr = d.liveBroker.Subscribe(ch)
		}
		recordError(span, subscribeErr)
		span.End()
		if subscribeErr != nil {
			return subscribeErr
		}
	}
	return nil
}

// Unsubscribe unsubscribes node from channels.
// Note: the interface signature does not include a context parameter (consistent with centrifuge.NodeBroker);
// context.Background() here is only used for tracing span creation, not affecting business logic.
func (d *DualBroker) Unsubscribe(channels ...string) error {
	for _, ch := range channels {
		ct, err := d.getChannelType(ch)
		if err != nil {
			return err
		}
		_, span := d.tracer.Start(context.Background(), "centrifugeplus.dualbroker.unsubscribe",
			trace.WithAttributes(
				AttributeChannel.String(ch),
				AttributeChannelType.String(ct.String()),
			),
		)

		var unsubscribeErr error
		switch ct {
		case Topic:
			unsubscribeErr = d.topicBroker.Unsubscribe(ch)
		default: // Live
			unsubscribeErr = d.liveBroker.Unsubscribe(ch)
		}

		recordError(span, unsubscribeErr)
		span.End()
		if unsubscribeErr != nil {
			return unsubscribeErr
		}

		// Clean up channelType registration to prevent memory leaks (only after underlying unsubscribe succeeds).
		d.channelTypes.Delete(ch)
	}
	return nil
}

// Publish publishes data to channel.
// Uses WithTimeout to prevent indefinite blocking if Redis is unresponsive.
// The 5s timeout matches PublishJoin/PublishLeave in topic_broker.go.
func (d *DualBroker) Publish(ch string, data []byte, opts centrifuge.PublishOptions) (centrifuge.PublishResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.PublishWithContext(ctx, ch, data, opts)
}

// PublishWithContext is like Publish but accepts a context for distributed tracing.
func (d *DualBroker) PublishWithContext(ctx context.Context, ch string, data []byte, opts centrifuge.PublishOptions) (result centrifuge.PublishResult, err error) {
	ct, getErr := d.getChannelType(ch)
	if getErr != nil {
		err = getErr
		return
	}
	ctx, span := d.tracer.Start(ctx, "centrifugeplus.dualbroker.publish",
		trace.WithAttributes(
			AttributeChannel.String(ch),
			AttributeChannelType.String(ct.String()),
		),
	)
	defer func() {
		recordError(span, err)
		span.End()
	}()

	switch ct {
	case Topic:
		result, err = d.topicBroker.PublishWithContext(ctx, ch, data, opts)
		return
	default: // Live
		// Known limitation: centrifuge.RedisBroker.Publish does not support context parameter;
		// it uses context.Background() internally, so the tracing chain breaks here.
		result, err = d.liveBroker.Publish(ch, data, opts)
		return
	}
}

// BatchIncrby batch pre-allocates offsets for multiple channels.
// Routes to TopicBroker.
func (d *DualBroker) BatchIncrby(ctx context.Context, reqs []ChannelIncrbyRequest) (map[string]centrifuge.StreamPosition, error) {
	return d.topicBroker.BatchIncrby(ctx, reqs)
}

// PublishWithUserOffset publishes using the caller's pre-allocated user_update offset.
// Topic channels route to TopicBroker.PublishWithUserOffset (unified offset).
// Live channels route to liveBroker.Publish (offset ignored — live doesn't use offset).
func (d *DualBroker) PublishWithUserOffset(ctx context.Context, ch string, data []byte, offset uint32, opts centrifuge.PublishOptions) (result centrifuge.PublishResult, err error) {
	ct, getErr := d.getChannelType(ch)
	if getErr != nil {
		err = getErr
		return
	}
	ctx, span := d.tracer.Start(ctx, "centrifugeplus.dualbroker.publish_user_update",
		trace.WithAttributes(
			AttributeChannel.String(ch),
			AttributeChannelType.String(ct.String()),
		),
	)
	defer func() {
		recordError(span, err)
		span.End()
	}()

	switch ct {
	case Topic:
		result, err = d.topicBroker.PublishWithUserOffset(ctx, ch, data, offset, opts)
		return
	default: // Live — offset is meaningless, directly use liveBroker.Publish
		result, err = d.liveBroker.Publish(ch, data, opts)
		return
	}
}

// PublishWithOffset publishes a message using a pre-allocated offset.
// Routes to TopicBroker.
func (d *DualBroker) PublishWithOffset(ctx context.Context, ch string, data []byte, opts centrifuge.PublishOptions, sp centrifuge.StreamPosition) error {
	return d.topicBroker.PublishWithOffset(ctx, ch, data, opts, sp)
}

// PublishEphemeral forces routing through liveBroker, pure PUB/SUB, no persistence to stream.
// Used for transient events like typing: loss is acceptable, no offset or recovery needed.
func (d *DualBroker) PublishEphemeral(ctx context.Context, ch string, data []byte, opts centrifuge.PublishOptions) error {
	_, span := d.tracer.Start(ctx, "centrifugeplus.dualbroker.publish_ephemeral",
		trace.WithAttributes(
			AttributeChannel.String(ch),
		),
	)
	defer span.End()

	_, err := d.liveBroker.Publish(ch, data, opts)
	recordError(span, err)
	return err
}

// PublishJoin publishes Join message to channel.
func (d *DualBroker) PublishJoin(ch string, info *centrifuge.ClientInfo) error {
	ct, err := d.getChannelType(ch)
	if err != nil {
		return err
	}
	_, span := d.tracer.Start(context.Background(), "centrifugeplus.dualbroker.publish_join",
		trace.WithAttributes(
			AttributeChannel.String(ch),
			AttributeChannelType.String(ct.String()),
		),
	)
	defer span.End()

	switch ct {
	case Topic:
		err := d.topicBroker.PublishJoin(ch, info)
		recordError(span, err)
		return err
	default: // Live
		err := d.liveBroker.PublishJoin(ch, info)
		recordError(span, err)
		return err
	}
}

// PublishLeave publishes Leave message to channel.
func (d *DualBroker) PublishLeave(ch string, info *centrifuge.ClientInfo) error {
	ct, err := d.getChannelType(ch)
	if err != nil {
		return err
	}
	_, span := d.tracer.Start(context.Background(), "centrifugeplus.dualbroker.publish_leave",
		trace.WithAttributes(
			AttributeChannel.String(ch),
			AttributeChannelType.String(ct.String()),
		),
	)
	defer span.End()

	switch ct {
	case Topic:
		err := d.topicBroker.PublishLeave(ch, info)
		recordError(span, err)
		return err
	default: // Live
		err := d.liveBroker.PublishLeave(ch, info)
		recordError(span, err)
		return err
	}
}

// History returns publications for channel.
func (d *DualBroker) History(ch string, opts centrifuge.HistoryOptions) (pubs []*centrifuge.Publication, sp centrifuge.StreamPosition, err error) {
	ct, getErr := d.getChannelType(ch)
	if getErr != nil {
		err = getErr
		return
	}
	_, span := d.tracer.Start(context.Background(), "centrifugeplus.dualbroker.history",
		trace.WithAttributes(
			AttributeChannel.String(ch),
			AttributeChannelType.String(ct.String()),
		),
	)
	defer func() {
		recordError(span, err)
		span.End()
	}()

	switch ct {
	case Topic:
		pubs, sp, err = d.topicBroker.History(ch, opts)
		return
	default: // Live
		pubs, sp, err = d.liveBroker.History(ch, opts)
		return
	}
}

// RemoveHistory removes history from channel.
func (d *DualBroker) RemoveHistory(ch string) error {
	ct, err := d.getChannelType(ch)
	if err != nil {
		return err
	}
	_, span := d.tracer.Start(context.Background(), "centrifugeplus.dualbroker.remove_history",
		trace.WithAttributes(
			AttributeChannel.String(ch),
			AttributeChannelType.String(ct.String()),
		),
	)
	defer span.End()

	switch ct {
	case Topic:
		err := d.topicBroker.RemoveHistory(ch)
		recordError(span, err)
		return err
	default: // Live
		err := d.liveBroker.RemoveHistory(ch)
		recordError(span, err)
		return err
	}
}

// Close closes both brokers, aggregating any errors.
func (d *DualBroker) Close(ctx context.Context) error {
	var errs []error
	if err := d.liveBroker.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("live broker: %w", err))
	}
	if err := d.topicBroker.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("topic broker: %w", err))
	}

	// Clear channelTypes to release all references.
	// Normal cleanup happens in Unsubscribe (per-channel Delete), but this
	// ensures no entries survive after the broker is shut down.
	d.channelTypes.Range(func(key, value any) bool {
		d.channelTypes.Delete(key)
		return true
	})

	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}

// IncrConversationOffset allocates the next conversation offset for a given session.
func (d *DualBroker) IncrConversationOffset(ctx context.Context, conversationID string) (uint32, error) {
	return d.topicBroker.IncrConversationOffset(ctx, conversationID)
}

// SetConversationOffset sets the offset value for a given session, used to initialize the counter in fork scenarios.
func (d *DualBroker) SetConversationOffset(ctx context.Context, conversationID string, value uint32) error {
	return d.topicBroker.SetConversationOffset(ctx, conversationID, value)
}

// Ensure DualBroker implements centrifuge.Broker
var _ centrifuge.Broker = (*DualBroker)(nil)

// Ensure DualBroker implements centrifuge.Closer
var _ centrifuge.Closer = (*DualBroker)(nil)
