// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/rtc-agent/server/internal/model"
)

// RedisBanPublisher publishes ban events via Redis Pub/Sub.
type RedisBanPublisher struct {
	rdb redis.UniversalClient
}

// NewRedisBanPublisher creates a new RedisBanPublisher.
func NewRedisBanPublisher(rdb redis.UniversalClient) *RedisBanPublisher {
	return &RedisBanPublisher{rdb: rdb}
}

// PublishBan publishes a ban/unban event to Redis.
func (p *RedisBanPublisher) PublishBan(ctx context.Context, userID uuid.UUID, action, reason string) error {
	if p.rdb == nil {
		return fmt.Errorf("redis client is required for ban publisher")
	}

	event := model.BanEvent{
		UserID: userID.String(),
		Action: action,
		Reason: reason,
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal ban event: %w", err)
	}

	if err := p.rdb.Publish(ctx, model.BanChannel, data).Err(); err != nil {
		return fmt.Errorf("publish ban event: %w", err)
	}

	return nil
}
