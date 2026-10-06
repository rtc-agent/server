// Package svc provides service-level components.
package svc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/centrifugal/centrifuge"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

const (
	// banChannel is the Redis Pub/Sub channel for user ban events.
	banChannel = "rtc:user_banned"
)

// BanEvent represents a user ban/unban event published via Redis Pub/Sub.
type BanEvent struct {
	UserID string `json:"user_id"`
	Action string `json:"action"` // "ban" or "unban"
	Reason string `json:"reason,omitempty"`
}

// BanWatcher listens for user ban events and disconnects users from Centrifuge.
type BanWatcher struct {
	node       *centrifuge.Node
	rdb        redis.UniversalClient
	sub        *redis.PubSub
	instanceID string
	mu         sync.Mutex
	closed     bool
}

// NewBanWatcher creates a BanWatcher that listens for ban events.
func NewBanWatcher(node *centrifuge.Node, rdb redis.UniversalClient, instanceID string) (*BanWatcher, error) {
	if rdb == nil {
		return nil, fmt.Errorf("redis client is required for ban watcher")
	}

	bw := &BanWatcher{
		node:       node,
		rdb:        rdb,
		instanceID: instanceID,
	}

	// Subscribe to ban channel
	bw.sub = rdb.Subscribe(context.Background(), banChannel)
	_, err := bw.sub.Receive(context.Background())
	if err != nil {
		return nil, fmt.Errorf("subscribe to ban channel: %w", err)
	}

	// Start background listener
	go bw.listen()

	return bw, nil
}

// PublishBan publishes a user ban/unban event to Redis.
func (bw *BanWatcher) PublishBan(ctx context.Context, userID uuid.UUID, action, reason string) error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.closed {
		return nil
	}

	event := BanEvent{
		UserID: userID.String(),
		Action: action,
		Reason: reason,
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal ban event: %w", err)
	}

	if err := bw.rdb.Publish(ctx, banChannel, data).Err(); err != nil {
		return fmt.Errorf("publish ban event: %w", err)
	}

	return nil
}

// listen processes incoming ban event notifications.
func (bw *BanWatcher) listen() {
	ch := bw.sub.Channel()
	for msg := range ch {
		var event BanEvent
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
