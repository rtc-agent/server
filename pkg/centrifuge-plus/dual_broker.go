package centrifugeplus

import (
	"context"
	"fmt"
	"sync"

	"github.com/centrifugal/centrifuge"
	"go.opentelemetry.io/otel/trace"
)

// DualBroker implements centrifuge.Broker interface.
// It routes messages to either RedisBroker (Live) or TopicBroker (Topic) based on channel type.
type DualBroker struct {
	liveBroker   *centrifuge.RedisBroker
	topicBroker  *TopicBroker
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
// 返回 error 而非默认值，避免未注册的频道被静默路由到 Live 导致消息丢失。
// 当 channel type 未显式注册时，根据频道名前缀自动推断并注册（处理发布先于订阅的场景）。
func (d *DualBroker) getChannelType(ch string) (ChannelType, error) {
	if v, ok := d.channelTypes.Load(ch); ok {
		return v.(ChannelType), nil
	}
	// Fallback: 根据前缀推断 channel type（topic: → Topic, live: → Live）
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
// 注意：接口签名不含 context 参数（与 centrifuge.NodeBroker 保持一致），
// 此处 context.Background() 仅用于 tracing span 创建，不影响业务逻辑。
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
// 注意：接口签名不含 context 参数（与 centrifuge.NodeBroker 保持一致），
// 此处 context.Background() 仅用于 tracing span 创建，不影响业务逻辑。
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

		// 清理 channelType 注册，防止内存泄漏（仅在底层 unsubscribe 成功后执行）
		d.channelTypes.Delete(ch)
	}
	return nil
}

// Publish publishes data to channel.
func (d *DualBroker) Publish(ch string, data []byte, opts centrifuge.PublishOptions) (centrifuge.PublishResult, error) {
	return d.PublishWithContext(context.Background(), ch, data, opts)
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
		// 已知限制：centrifuge.RedisBroker.Publish 不支持 context 参数，
		// 内部使用 context.Background()，tracing 链路在此断裂。
		result, err = d.liveBroker.Publish(ch, data, opts)
		return
	}
}

// BatchIncrby batch pre-allocates offsets for multiple channels.
// Routes to TopicBroker.
func (d *DualBroker) BatchIncrby(ctx context.Context, reqs []ChannelIncrbyRequest) (map[string]centrifuge.StreamPosition, error) {
	return d.topicBroker.BatchIncrby(ctx, reqs)
}

// PublishWithOffset publishes a message using a pre-allocated offset.
// Routes to TopicBroker.
func (d *DualBroker) PublishWithOffset(ctx context.Context, ch string, data []byte, opts centrifuge.PublishOptions, sp centrifuge.StreamPosition) error {
	return d.topicBroker.PublishWithOffset(ctx, ch, data, opts, sp)
}

// PublishEphemeral 强制走 liveBroker，纯 PUB/SUB，不持久化到 stream。
// 用于 typing 等瞬态事件：丢失可接受，不需要 offset 和 recovery。
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
	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}

// IncrConversationOffset 为指定会话分配下一个 conversation offset
func (d *DualBroker) IncrConversationOffset(ctx context.Context, conversationID string) (uint32, error) {
	return d.topicBroker.IncrConversationOffset(ctx, conversationID)
}

// SetConversationOffset 设置指定会话的 offset 值，用于 fork 等场景初始化计数器
func (d *DualBroker) SetConversationOffset(ctx context.Context, conversationID string, value uint32) error {
	return d.topicBroker.SetConversationOffset(ctx, conversationID, value)
}

// Ensure DualBroker implements centrifuge.Broker
var _ centrifuge.Broker = (*DualBroker)(nil)

// Ensure DualBroker implements centrifuge.Closer
var _ centrifuge.Closer = (*DualBroker)(nil)
