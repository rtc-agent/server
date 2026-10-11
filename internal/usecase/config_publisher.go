package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
)

// RedisConfigPublisher publishes config update events via Redis Pub/Sub.
// This enables multi-instance deployments to keep their config caches in sync:
// when one instance updates a config, it publishes an event that all instances
// (including itself) receive and use to invalidate their local caches.
type RedisConfigPublisher struct {
	rdb        redis.UniversalClient
	instanceID string // unique identifier for this instance (for debugging)
}

// NewRedisConfigPublisher creates a new RedisConfigPublisher.
func NewRedisConfigPublisher(rdb redis.UniversalClient) *RedisConfigPublisher {
	hostname, _ := os.Hostname()
	pid := os.Getpid()
	return &RedisConfigPublisher{
		rdb:        rdb,
		instanceID: fmt.Sprintf("%s-%d", hostname, pid),
	}
}

// PublishConfigUpdate publishes a config update event to Redis.
// This should be called after a config is successfully updated in the database.
// Returns error immediately on failure - no retry logic.
// Redis is critical infrastructure; failures should be handled at the caller level.
func (p *RedisConfigPublisher) PublishConfigUpdate(ctx context.Context, key string, userID *uuid.UUID, version int, operation string) error {
	if p.rdb == nil {
		return fmt.Errorf("redis client is required for config publisher")
	}

	var userIDStr *string
	if userID != nil {
		s := userID.String()
		userIDStr = &s
	}

	event := model.ConfigUpdateEvent{
		Key:       key,
		UserID:    userIDStr,
		Version:   version,
		Operation: operation,
		Source:    p.instanceID,
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal config update event: %w", err)
	}

	if err := p.rdb.Publish(ctx, model.ConfigChannel, data).Err(); err != nil {
		return fmt.Errorf("publish config update event: %w", err)
	}

	logger.Debug(ctx, "config.update_published",
		zap.String("key", key),
		zap.Int("version", version),
		zap.String("operation", operation),
		zap.String("source", p.instanceID))

	return nil
}

// ConfigUpdateHandler is a function that handles config update events.
// Implemented by CachedConfigProvider to invalidate cache entries.
type ConfigUpdateHandler func(event model.ConfigUpdateEvent)

// RedisConfigSubscriber subscribes to config update events from Redis Pub/Sub
// and invokes registered handlers to invalidate local caches.
type RedisConfigSubscriber struct {
	rdb      redis.UniversalClient
	handlers []ConfigUpdateHandler
}

// NewRedisConfigSubscriber creates a new RedisConfigSubscriber.
func NewRedisConfigSubscriber(rdb redis.UniversalClient) *RedisConfigSubscriber {
	return &RedisConfigSubscriber{
		rdb:      rdb,
		handlers: make([]ConfigUpdateHandler, 0),
	}
}

// RegisterHandler adds a handler that will be called when config update events are received.
// Typically, the CachedConfigProvider registers itself as a handler to invalidate its cache.
func (s *RedisConfigSubscriber) RegisterHandler(handler ConfigUpdateHandler) {
	s.handlers = append(s.handlers, handler)
}

// Start begins listening for config update events in a background goroutine.
// This should be called once during server startup.
// Returns a function that can be called to stop the subscriber.
func (s *RedisConfigSubscriber) Start(ctx context.Context) func() {
	if s.rdb == nil {
		logger.Warn(ctx, "config.subscriber.no_redis", zap.String("message", "Redis client not configured, config updates will not be propagated across instances"))
		return func() {}
	}

	pubsub := s.rdb.Subscribe(ctx, model.ConfigChannel)

	// Wait for subscription confirmation.
	_, err := pubsub.Receive(ctx)
	if err != nil {
		logger.Error(ctx, "config.subscriber.receive_error",
			zap.Error(err))
		return func() {}
	}

	logger.Info(ctx, "config.subscriber.started",
		zap.String("channel", model.ConfigChannel))

	// Process messages in a goroutine.
	logger.SafeGo("config.subscriber", func() {
		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				logger.Info(ctx, "config.subscriber.stopping",
					zap.String("reason", "context cancelled"))
				// pubsub.Close errors are logged but don't prevent shutdown.
				// Redis connections may already be broken when context is cancelled.
				if err := pubsub.Close(); err != nil {
					logger.Warn(ctx, "config.subscriber.close_failed",
						zap.Error(err))
				}
				return
			case msg, ok := <-ch:
				if !ok {
					logger.Info(ctx, "config.subscriber.channel_closed")
					return
				}
				s.handleMessage(ctx, msg)
			}
		}
	})

	return func() {
		// pubsub.Close errors are logged but don't prevent cleanup.
		// This is a best-effort cleanup; Redis will eventually timeout the connection.
		if err := pubsub.Close(); err != nil {
			logger.Warn(ctx, "config.subscriber.cleanup_close_failed",
				zap.Error(err))
		}
	}
}

// handleMessage processes a single config update event message.
func (s *RedisConfigSubscriber) handleMessage(ctx context.Context, msg *redis.Message) {
	var event model.ConfigUpdateEvent
	if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
		logger.Error(ctx, "config.subscriber.unmarshal_error",
			zap.Error(err),
			zap.String("payload", msg.Payload))
		return
	}

	logger.Debug(ctx, "config.subscriber.received",
		zap.String("key", event.Key),
		zap.Int("version", event.Version),
		zap.String("operation", event.Operation),
		zap.String("source", event.Source))

	// Invoke all registered handlers.
	for _, handler := range s.handlers {
		handler(event)
	}
}
