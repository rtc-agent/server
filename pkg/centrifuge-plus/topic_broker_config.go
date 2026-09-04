package centrifugeplus

import "time"

// TopicBrokerConfig is the configuration for TopicBroker.
type TopicBrokerConfig struct {
	// Prefix is the Redis key prefix.
	Prefix string

	// RedisAddr is the Redis server address (host:port).
	RedisAddr string

	// RedisPassword is the Redis password (optional).
	RedisPassword string

	// RedisDB is the Redis database number.
	RedisDB int

	// HistoryStore is the implementation for querying message history.
	HistoryStore HistoryStore

	// HistoryMetaTTL is the TTL for the meta key (epoch and top_offset).
	// If 0, no TTL is set.
	HistoryMetaTTL time.Duration

	// Logger is the logger used by the broker.
	// If nil, a default logger backed by log.Printf is used.
	Logger Logger

	// Tracing configures distributed tracing.
	// When disabled, all tracing operations are no-ops with zero overhead.
	Tracing TracingConfig
}
