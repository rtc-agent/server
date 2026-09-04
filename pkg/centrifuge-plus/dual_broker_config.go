package centrifugeplus

import "github.com/centrifugal/centrifuge"

// DualBrokerConfig is the configuration for DualBroker.
type DualBrokerConfig struct {
	// Live is the configuration for Live mode channels (using RedisBroker).
	Live centrifuge.RedisBrokerConfig

	// Topic is the configuration for Topic mode channels (using TopicBroker).
	Topic TopicBrokerConfig
}
