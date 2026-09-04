package centrifugeplus

// ChannelType defines the type of a channel.
type ChannelType int

const (
	// Live channels are not persisted, messages are only delivered to online subscribers.
	// Uses centrifuge's built-in RedisBroker.
	Live ChannelType = iota

	// Topic channels are persisted and support real-time push.
	// Messages are stored in history stream for offline recovery.
	Topic
)

// String returns the string representation of the ChannelType.
func (ct ChannelType) String() string {
	switch ct {
	case Topic:
		return "topic"
	default:
		return "live"
	}
}
