package webfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// CacheSyncMessage represents a cache synchronization message.
type CacheSyncMessage struct {
	Action    string    `json:"action"` // "set", "delete"
	Key       string    `json:"key"`
	Value     string    `json:"value,omitempty"` // Only for "set"
	Timestamp time.Time `json:"timestamp"`
	SourceID  string    `json:"source_id"` // Instance ID to avoid echo
}

// DistributedCacheSync synchronizes cache across instances using Redis Pub/Sub.
type DistributedCacheSync struct {
	redisClient redis.UniversalClient
	pubSub      *redis.PubSub
	channel     string
	sourceID    string // Unique ID for this instance
	logger      *zap.Logger
	onSet       func(ctx context.Context, key, value string)
	onDelete    func(ctx context.Context, key string)
	stopCh      chan struct{}
}

// NewDistributedCacheSync creates a new distributed cache sync.
func NewDistributedCacheSync(
	redisClient redis.UniversalClient,
	channel string,
	sourceID string,
	logger *zap.Logger,
) *DistributedCacheSync {
	if logger == nil {
		logger = zap.NewNop()
	}
	if channel == "" {
		channel = "webfetch:cache:sync"
	}
	return &DistributedCacheSync{
		redisClient: redisClient,
		channel:     channel,
		sourceID:    sourceID,
		logger:      logger,
		stopCh:      make(chan struct{}),
	}
}

// Start begins listening for cache sync messages.
func (s *DistributedCacheSync) Start(ctx context.Context) error {
	if s.redisClient == nil {
		return fmt.Errorf("redis client not configured")
	}

	s.pubSub = s.redisClient.Subscribe(ctx, s.channel)

	// Wait for subscription confirmation.
	_, err := s.pubSub.Receive(ctx)
	if err != nil {
		return fmt.Errorf("subscribe to channel: %w", err)
	}

	s.logger.Info("distributed cache sync started",
		zap.String("channel", s.channel),
		zap.String("source_id", s.sourceID))

	// Start listening for messages in background.
	go s.listenLoop(ctx)

	return nil
}

// Stop stops listening for cache sync messages.
func (s *DistributedCacheSync) Stop() error {
	if s.pubSub != nil {
		close(s.stopCh)
		return s.pubSub.Close()
	}
	return nil
}

// OnSet registers a callback for cache set operations from other instances.
func (s *DistributedCacheSync) OnSet(fn func(ctx context.Context, key, value string)) {
	s.onSet = fn
}

// OnDelete registers a callback for cache delete operations from other instances.
func (s *DistributedCacheSync) OnDelete(fn func(ctx context.Context, key string)) {
	s.onDelete = fn
}

// PublishSet publishes a cache set operation to other instances.
func (s *DistributedCacheSync) PublishSet(ctx context.Context, key, value string) error {
	if s.redisClient == nil {
		return nil
	}

	msg := CacheSyncMessage{
		Action:    "set",
		Key:       key,
		Value:     value,
		Timestamp: time.Now(),
		SourceID:  s.sourceID,
	}

	return s.publish(ctx, msg)
}

// PublishDelete publishes a cache delete operation to other instances.
func (s *DistributedCacheSync) PublishDelete(ctx context.Context, key string) error {
	if s.redisClient == nil {
		return nil
	}

	msg := CacheSyncMessage{
		Action:    "delete",
		Key:       key,
		Timestamp: time.Now(),
		SourceID:  s.sourceID,
	}

	return s.publish(ctx, msg)
}

// publish sends a message to the sync channel.
func (s *DistributedCacheSync) publish(ctx context.Context, msg CacheSyncMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	return s.redisClient.Publish(ctx, s.channel, data).Err()
}

// listenLoop listens for incoming sync messages.
func (s *DistributedCacheSync) listenLoop(ctx context.Context) {
	ch := s.pubSub.Channel()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			s.handleMessage(ctx, msg)
		}
	}
}

// handleMessage processes an incoming sync message.
func (s *DistributedCacheSync) handleMessage(ctx context.Context, msg *redis.Message) {
	var syncMsg CacheSyncMessage
	if err := json.Unmarshal([]byte(msg.Payload), &syncMsg); err != nil {
		s.logger.Warn("failed to unmarshal cache sync message", zap.Error(err))
		return
	}

	// Ignore messages from ourselves.
	if syncMsg.SourceID == s.sourceID {
		return
	}

	s.logger.Debug("received cache sync message",
		zap.String("action", syncMsg.Action),
		zap.String("key", syncMsg.Key),
		zap.String("source", syncMsg.SourceID))

	switch syncMsg.Action {
	case "set":
		if s.onSet != nil {
			s.onSet(ctx, syncMsg.Key, syncMsg.Value)
		}
	case "delete":
		if s.onDelete != nil {
			s.onDelete(ctx, syncMsg.Key)
		}
	default:
		s.logger.Warn("unknown cache sync action", zap.String("action", syncMsg.Action))
	}
}
