package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

// handlerRegistration wraps a handler with its metadata for metrics and monitoring.
type handlerRegistration struct {
	handler ConfigUpdateHandler
	name    string
}

// subscriberMetrics holds Prometheus metrics for config subscriber operations.
type subscriberMetrics struct {
	handlerDuration *prometheus.HistogramVec // labels: handler_name
	handlerErrors   prometheus.Counter
	handlerTimeouts prometheus.Counter
}

// global subscriberMetrics instance (promauto handles registration).
var globalSubscriberMetrics = newSubscriberMetrics()

// newSubscriberMetrics creates and registers all subscriber metrics.
func newSubscriberMetrics() *subscriberMetrics {
	return &subscriberMetrics{
		handlerDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "config_subscriber",
			Name:      "handler_duration_seconds",
			Help:      "Duration of config update handler execution in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"handler_name"}),
		handlerErrors: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "config_subscriber",
			Name:      "handler_errors_total",
			Help:      "Total number of config update handler execution errors.",
		}),
		handlerTimeouts: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "config_subscriber",
			Name:      "handler_timeouts_total",
			Help:      "Total number of config update handler timeouts.",
		}),
	}
}

// RedisConfigSubscriber subscribes to config update events from Redis Pub/Sub
// and invokes registered handlers to invalidate local caches.
// Handlers are executed asynchronously with timeout to prevent blocking message processing.
type RedisConfigSubscriber struct {
	rdb            redis.UniversalClient
	handlers       []handlerRegistration
	handlerTimeout time.Duration // max duration per handler execution
}

// NewRedisConfigSubscriber creates a new RedisConfigSubscriber.
func NewRedisConfigSubscriber(rdb redis.UniversalClient) *RedisConfigSubscriber {
	return &RedisConfigSubscriber{
		rdb:            rdb,
		handlers:       make([]handlerRegistration, 0),
		handlerTimeout: 5 * time.Second, // default timeout for handler execution
	}
}

// SetHandlerTimeout sets the maximum duration allowed for each handler execution.
// Default is 5 seconds. This prevents slow handlers from blocking message processing.
func (s *RedisConfigSubscriber) SetHandlerTimeout(timeout time.Duration) {
	s.handlerTimeout = timeout
}

// RegisterHandler adds a handler that will be called when config update events are received.
// Typically, the CachedConfigProvider registers itself as a handler to invalidate its cache.
// The name parameter is used for metrics and logging.
func (s *RedisConfigSubscriber) RegisterHandler(name string, handler ConfigUpdateHandler) {
	s.handlers = append(s.handlers, handlerRegistration{
		handler: handler,
		name:    name,
	})
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

	// Invoke all registered handlers asynchronously with timeout control.
	// This prevents slow handlers from blocking message processing and ensures
	// that config updates are processed promptly even if some handlers are slow.
	// Note: We cannot forcibly stop a goroutine in Go, so handlers that exceed
	// the timeout will continue running in the background. Handlers should be
	// designed to complete quickly (e.g., cache invalidation, not network calls).
	var wg sync.WaitGroup
	for _, reg := range s.handlers {
		wg.Add(1)
		go func(reg handlerRegistration) {
			defer wg.Done()
			start := time.Now()

			// Use a timer to detect timeout.
			// The handler itself continues running if it exceeds the timeout,
			// but we log and move on to prevent blocking other handlers.
			done := make(chan struct{})
			go func() {
				defer close(done)
				reg.handler(event)
			}()

			// Wait for completion or timeout.
			timer := time.NewTimer(s.handlerTimeout)
			defer timer.Stop()

			select {
			case <-done:
				duration := time.Since(start).Seconds()
				globalSubscriberMetrics.handlerDuration.WithLabelValues(reg.name).Observe(duration)
				logger.Debug(ctx, "config.subscriber.handler_completed",
					zap.String("handler", reg.name),
					zap.Duration("duration", time.Since(start)))
			case <-timer.C:
				globalSubscriberMetrics.handlerTimeouts.Inc()
				logger.Error(ctx, "config.subscriber.handler_timeout",
					zap.String("handler", reg.name),
					zap.Duration("timeout", s.handlerTimeout),
					zap.Duration("elapsed", time.Since(start)),
					zap.String("message", "handler continues running in background"))
			}
		}(reg)
	}

	// Wait for all handlers to complete (with timeout already enforced per-handler).
	// We use a separate timeout for the overall wait to prevent blocking indefinitely.
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		// All handlers completed.
	case <-time.After(s.handlerTimeout * 2):
		// This should not happen if per-handler timeouts work correctly,
		// but serves as a safety net.
		globalSubscriberMetrics.handlerErrors.Inc()
		logger.Error(ctx, "config.subscriber.wait_timeout",
			zap.Duration("timeout", s.handlerTimeout*2),
			zap.Int("handler_count", len(s.handlers)))
	}
}
