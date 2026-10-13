// Package svc provides service-level components.
package svc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
)

// BanWatcherConfig holds configuration for BanWatcher reconnection behavior.
type BanWatcherConfig struct {
	// MaxRetries is the maximum number of reconnection attempts.
	// 0 means unlimited retries (default behavior for backward compatibility).
	MaxRetries int
	// InitialBackoff is the initial delay between reconnection attempts.
	InitialBackoff time.Duration
	// MaxBackoff is the maximum delay between reconnection attempts.
	MaxBackoff time.Duration
}

// DefaultBanWatcherConfig returns the default configuration.
func DefaultBanWatcherConfig() BanWatcherConfig {
	return BanWatcherConfig{
		MaxRetries:     0, // Unlimited retries by default
		InitialBackoff: time.Second,
		MaxBackoff:     30 * time.Second,
	}
}

// BanWatcher listens for user ban events and disconnects users from Centrifuge.
type BanWatcher struct {
	node    *centrifuge.Node
	rdb     redis.UniversalClient
	sub     *redis.PubSub
	mu      sync.Mutex
	closed  bool
	baseCtx context.Context
	config  BanWatcherConfig
}

// NewBanWatcher creates a BanWatcher that listens for ban events.
// Optional config can be provided to customize reconnection behavior.
// If no config is provided, default values are used.
func NewBanWatcher(node *centrifuge.Node, rdb redis.UniversalClient, config ...BanWatcherConfig) (*BanWatcher, error) {
	if rdb == nil {
		return nil, fmt.Errorf("redis client is required for ban watcher")
	}

	// Use provided config or default
	cfg := DefaultBanWatcherConfig()
	if len(config) > 0 {
		cfg = config[0]
		// Apply defaults for zero values
		if cfg.InitialBackoff == 0 {
			cfg.InitialBackoff = time.Second
		}
		if cfg.MaxBackoff == 0 {
			cfg.MaxBackoff = 30 * time.Second
		}
	}

	bw := &BanWatcher{
		node:    node,
		rdb:     rdb,
		baseCtx: context.Background(),
		config:  cfg,
	}

	// Subscribe to ban channel
	if err := bw.subscribe(); err != nil {
		return nil, err
	}

	// Start background listener with reconnection
	logger.SafeGo("ban_watcher.listen", func() {
		bw.listenWithReconnect()
	})

	return bw, nil
}

// subscribe creates a new Redis Pub/Sub subscription.
// Must be called with bw.mu held.
func (bw *BanWatcher) subscribe() error {
	// Close old subscription to prevent resource leak
	if bw.sub != nil {
		// Close errors are logged but typically not actionable.
		// The subscription might already be closed or the connection might be broken.
		if err := bw.sub.Close(); err != nil {
			logger.Warn(bw.baseCtx, "ban_watcher.old_subscription_close_failed", zap.Error(err))
		}
	}
	// Use a timeout for the subscribe operation to prevent blocking indefinitely
	// if the Redis connection hangs.
	subCtx, subCancel := context.WithTimeout(bw.baseCtx, 10*time.Second)
	defer subCancel()
	sub := bw.rdb.Subscribe(subCtx, model.BanChannel)
	_, err := sub.Receive(subCtx)
	if err != nil {
		return fmt.Errorf("subscribe to ban channel: %w", err)
	}
	bw.sub = sub
	return nil
}

// listenWithReconnect runs the listener with automatic reconnection on failure.
// Uses cenkalti/backoff for exponential backoff with jitter.
func (bw *BanWatcher) listenWithReconnect() {
	retryCount := 0

	for {
		bw.mu.Lock()
		if bw.closed {
			bw.mu.Unlock()
			return
		}
		bw.mu.Unlock()

		// Run the listener - blocks until channel closes
		bw.listen()

		// Check if we should exit
		bw.mu.Lock()
		if bw.closed {
			bw.mu.Unlock()
			return
		}
		bw.mu.Unlock()

		// Check if max retries reached (0 means unlimited)
		if bw.config.MaxRetries > 0 && retryCount >= bw.config.MaxRetries {
			logger.Error(bw.baseCtx, "ban_watcher.max_retries_reached_giving_up",
				zap.Int("max_retries", bw.config.MaxRetries),
				zap.Int("total_attempts", retryCount))
			// Exit the reconnection loop gracefully
			// The watcher will remain in a degraded state until manually restarted or server restarts
			return
		}

		// Connection lost, attempt to reconnect with exponential backoff
		bo := backoff.NewExponentialBackOff()
		bo.InitialInterval = bw.config.InitialBackoff
		bo.MaxInterval = bw.config.MaxBackoff

		retryCount++
		logger.Warn(bw.baseCtx, "ban_watcher.connection_lost_reconnecting",
			zap.Int("retry_attempt", retryCount),
			zap.Int("max_retries", bw.config.MaxRetries),
			zap.Duration("initial_backoff", bo.InitialInterval),
			zap.Duration("max_backoff", bo.MaxInterval))

		_, err := backoff.Retry(bw.baseCtx, func() (struct{}, error) {
			bw.mu.Lock()
			if bw.closed {
				bw.mu.Unlock()
				return struct{}{}, backoff.Permanent(fmt.Errorf("watcher closed"))
			}

			if err := bw.subscribe(); err != nil {
				bw.mu.Unlock()
				logger.Warn(bw.baseCtx, "ban_watcher.resubscribe_failed",
					zap.Int("retry_attempt", retryCount),
					zap.Error(err))
				return struct{}{}, err
			}
			bw.mu.Unlock()

			logger.Info(bw.baseCtx, "ban_watcher.resubscribed_successfully",
				zap.Int("retry_attempts", retryCount))
			return struct{}{}, nil
		},
			backoff.WithBackOff(bo),
			backoff.WithMaxTries(uint(bw.config.MaxRetries)), // 0 means unlimited
		)

		if err != nil {
			// Permanent error or closed watcher
			logger.Error(bw.baseCtx, "ban_watcher.reconnection_failed",
				zap.Int("total_attempts", retryCount),
				zap.Error(err))
			return
		}

		// Successfully resubscribed — reset retry counter and loop back to bw.listen()
		retryCount = 0
		logger.Info(bw.baseCtx, "ban_watcher.connection_restored")
	}
}

// listen processes incoming ban event notifications.
func (bw *BanWatcher) listen() {
	bw.mu.Lock()
	if bw.sub == nil {
		bw.mu.Unlock()
		return
	}
	ch := bw.sub.Channel()
	bw.mu.Unlock()

	for msg := range ch {
		var event model.BanEvent
		if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
			logger.Warn(bw.baseCtx, "ban_watcher.invalid_message",
				zap.Error(err), zap.String("payload", msg.Payload))
			continue
		}

		if event.Action == "ban" {
			userIDStr := event.UserID
			disconnect := centrifuge.Disconnect{
				Code:   4501,
				Reason: "account banned",
			}

			// Parse userID string to UUID for cache update
			userID, err := uuid.Parse(userIDStr)
			if err != nil {
				logger.Error(bw.baseCtx, "ban_watcher.invalid_user_id",
					zap.String("user_id", userIDStr), zap.Error(err))
				continue
			}

			// Update ban cache to ensure HTTP API also rejects this user immediately
			// This is a defensive measure in case BanUser's SetBanCache failed
			middleware.SetBanCache(bw.baseCtx, userID, true)

			if err := bw.node.Disconnect(userIDStr, centrifuge.WithCustomDisconnect(disconnect)); err != nil {
				logger.Error(bw.baseCtx, "ban_watcher.disconnect_failed",
					zap.String("user_id", userIDStr), zap.Error(err))
			} else {
				logger.Info(bw.baseCtx, "ban_watcher.user_disconnected",
					zap.String("user_id", userIDStr), zap.String("reason", event.Reason))
			}
		} else if event.Action == "unban" {
			// Handle unban events: invalidate ban cache on all instances
			// This ensures the unban takes effect immediately across the distributed system
			userIDStr := event.UserID
			userID, err := uuid.Parse(userIDStr)
			if err != nil {
				logger.Error(bw.baseCtx, "ban_watcher.invalid_user_id",
					zap.String("user_id", userIDStr), zap.Error(err))
				continue
			}

			// Invalidate ban cache to allow immediate reconnection
			middleware.InvalidateBanCache(bw.baseCtx, userID)
			logger.Info(bw.baseCtx, "ban_watcher.ban_cache_invalidated",
				zap.String("user_id", userIDStr))
		}
	}
}

// Close stops the ban watcher and unsubscribes from Redis.
func (bw *BanWatcher) Close() error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.closed {
		return nil
	}
	bw.closed = true

	// 添加超时保护，防止 Redis 连接阻塞导致 Close 永久阻塞
	// 5 秒超时足够覆盖正常的 Close 操作（毫秒级），远小于服务器关闭超时（30s）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- bw.sub.Close()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// 接受超时，继续关闭流程
		// 泄漏的 goroutine 会在 Redis 连接最终关闭时退出
		logger.Warn(bw.baseCtx, "ban_watcher.subscription_close_timeout",
			zap.String("timeout", "5s"))
		return nil
	}
}
