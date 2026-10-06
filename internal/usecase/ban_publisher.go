// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
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

// RedisBanPublisher publishes ban events via Redis Pub/Sub.
type RedisBanPublisher struct {
	rdb *redis.Client
}

// NewRedisBanPublisher creates a new RedisBanPublisher.
func NewRedisBanPublisher(rdb *redis.Client) *RedisBanPublisher {
	return &RedisBanPublisher{rdb: rdb}
}

// PublishBan publishes a ban/unban event to Redis.
func (p *RedisBanPublisher) PublishBan(ctx context.Context, userID uuid.UUID, action, reason string) error {
	if p.rdb == nil {
		return fmt.Errorf("redis client is required for ban publisher")
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

	if err := p.rdb.Publish(ctx, banChannel, data).Err(); err != nil {
		return fmt.Errorf("publish ban event: %w", err)
	}

	return nil
}
