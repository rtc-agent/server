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
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
)

// BanWatcher listens for user ban events and disconnects users from Centrifuge.
type BanWatcher struct {
	node    *centrifuge.Node
	rdb     redis.UniversalClient
	sub     *redis.PubSub
	mu      sync.Mutex
	closed  bool
	baseCtx context.Context
}

// NewBanWatcher creates a BanWatcher that listens for ban events.
func NewBanWatcher(node *centrifuge.Node, rdb redis.UniversalClient) (*BanWatcher, error) {
	if rdb == nil {
		return nil, fmt.Errorf("redis client is required for ban watcher")
	}

	bw := &BanWatcher{
		node:    node,
		rdb:     rdb,
		baseCtx: context.Background(),
	}

	// Subscribe to ban channel
	if err := bw.subscribe(); err != nil {
		return nil, err
	}

	// Start background listener with reconnection
	go bw.listenWithReconnect()

	return bw, nil
}

// subscribe creates a new Redis Pub/Sub subscription.
func (bw *BanWatcher) subscribe() error {
	// Close old subscription to prevent resource leak
	if bw.sub != nil {
		_ = bw.sub.Close()
	}
	bw.sub = bw.rdb.Subscribe(bw.baseCtx, model.BanChannel)
	_, err := bw.sub.Receive(bw.baseCtx)
	if err != nil {
		return fmt.Errorf("subscribe to ban channel: %w", err)
	}
	return nil
}

// listenWithReconnect runs the listener with automatic reconnection on failure.
// Uses cenkalti/backoff for exponential backoff with jitter.
func (bw *BanWatcher) listenWithReconnect() {
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

		// Connection lost, attempt to reconnect with exponential backoff
		bo := backoff.NewExponentialBackOff()
		bo.InitialInterval = time.Second
		bo.MaxInterval = 30 * time.Second

		logger.Warn(bw.baseCtx, "ban_watcher.connection_lost_reconnecting",
			zap.Duration("initial_backoff", bo.InitialInterval))

		_, err := backoff.Retry(bw.baseCtx, func() (struct{}, error) {
			bw.mu.Lock()
			if bw.closed {
				bw.mu.Unlock()
				return struct{}{}, backoff.Permanent(fmt.Errorf("watcher closed"))
			}

			if err := bw.subscribe(); err != nil {
				bw.mu.Unlock()
				logger.Error(bw.baseCtx, "ban_watcher.resubscribe_failed",
					zap.Error(err))
				return struct{}{}, err
			}
			bw.mu.Unlock()

			logger.Info(bw.baseCtx, "ban_watcher.resubscribed_successfully")
			return struct{}{}, nil
		},
			backoff.WithBackOff(bo),
			backoff.WithMaxTries(0), // Unlimited retries
		)

		if err != nil {
			// Permanent error or closed watcher
			return
		}

		// Successfully resubscribed — loop back to bw.listen() at the top,
		// which will read from the newly created subscription.
	}
}

// listen processes incoming ban event notifications.
func (bw *BanWatcher) listen() {
	ch := bw.sub.Channel()
	for msg := range ch {
		var event model.BanEvent
		if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
			logger.Warn(bw.baseCtx, "ban_watcher.invalid_message",
				zap.Error(err), zap.String("payload", msg.Payload))
			continue
		}

		if event.Action == "ban" {
			userID := event.UserID
			disconnect := centrifuge.Disconnect{
				Code:   4501,
				Reason: "account banned",
			}

			if err := bw.node.Disconnect(userID, centrifuge.WithCustomDisconnect(disconnect)); err != nil {
				logger.Error(bw.baseCtx, "ban_watcher.disconnect_failed",
					zap.String("user_id", userID), zap.Error(err))
			} else {
				logger.Info(bw.baseCtx, "ban_watcher.user_disconnected",
					zap.String("user_id", userID), zap.String("reason", event.Reason))
			}
		}
		// For "unban" events, we don't need to do anything - the user can reconnect normally
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
	return bw.sub.Close()
}
