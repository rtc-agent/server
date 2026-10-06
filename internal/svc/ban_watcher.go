// Package svc provides service-level components.
package svc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
)

// BanWatcher listens for user ban events and disconnects users from Centrifuge.
type BanWatcher struct {
	node   *centrifuge.Node
	rdb    redis.UniversalClient
	sub    *redis.PubSub
	mu     sync.Mutex
	closed bool
}

// NewBanWatcher creates a BanWatcher that listens for ban events.
func NewBanWatcher(node *centrifuge.Node, rdb redis.UniversalClient) (*BanWatcher, error) {
	if rdb == nil {
		return nil, fmt.Errorf("redis client is required for ban watcher")
	}

	bw := &BanWatcher{
		node: node,
		rdb:  rdb,
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
	bw.sub = bw.rdb.Subscribe(context.Background(), model.BanChannel)
	_, err := bw.sub.Receive(context.Background())
	if err != nil {
		return fmt.Errorf("subscribe to ban channel: %w", err)
	}
	return nil
}

// listenWithReconnect runs the listener with automatic reconnection on failure.
func (bw *BanWatcher) listenWithReconnect() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		bw.mu.Lock()
		if bw.closed {
			bw.mu.Unlock()
			return
		}
		bw.mu.Unlock()

		// Run the listener
		bw.listen()

		// Check if we should exit
		bw.mu.Lock()
		if bw.closed {
			bw.mu.Unlock()
			return
		}
		bw.mu.Unlock()

		// Connection lost, attempt to reconnect
		logger.Warn(context.Background(), "ban_watcher.connection_lost_reconnecting",
			zap.Duration("backoff", backoff))

		select {
		case <-time.After(backoff):
			// Exponential backoff
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}

			// Try to resubscribe
			bw.mu.Lock()
			if bw.closed {
				bw.mu.Unlock()
				return
			}
			if err := bw.subscribe(); err != nil {
				logger.Error(context.Background(), "ban_watcher.resubscribe_failed",
					zap.Error(err))
				bw.mu.Unlock()
				continue
			}
			bw.mu.Unlock()

			logger.Info(context.Background(), "ban_watcher.resubscribed_successfully")
			backoff = time.Second // Reset backoff on success

		case <-bw.sub.Channel():
			// Channel closed, will retry immediately
			backoff = time.Second
		}
	}
}

// listen processes incoming ban event notifications.
func (bw *BanWatcher) listen() {
	ch := bw.sub.Channel()
	for msg := range ch {
		var event model.BanEvent
		if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
			logger.Warn(context.Background(), "ban_watcher.invalid_message",
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
				logger.Error(context.Background(), "ban_watcher.disconnect_failed",
					zap.String("user_id", userID), zap.Error(err))
			} else {
				logger.Info(context.Background(), "ban_watcher.user_disconnected",
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
