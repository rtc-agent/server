package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rtc-agent/server/internal/infra/cache"
)

// =============================================================================
// Stream store helper
// =============================================================================

// getStreamStore returns a StreamStore for buffering streaming message chunks
// in Redis. Returns nil if the Redis client is not available.
//
// TODO: The StreamStore is currently defined in internal/worker/stream_store.go.
// It should be moved to a shared package so both worker and agent can use it.
// For now, we create a lightweight Redis-based chunk buffer inline.
// NewStreamStore creates a stream store accessor backed by Redis.
// The returned value satisfies updates.StreamStoreAccessor and can be injected
// into UpdatePublisher via SetStreamStore. It uses the same Redis key format
// (cache.MessageStream) as the stream buffering logic in this package.
func NewStreamStore(rdb redis.UniversalClient, chunkTTL time.Duration) *StreamStore {
	return &StreamStore{rdb: rdb, chunkTTL: chunkTTL}
}

func (h *helpers) getStreamStore() *StreamStore {
	return &StreamStore{rdb: h.rdb, chunkTTL: h.streamChunkTTL}
}

// StreamStore is a lightweight Redis-based buffer for streaming message
// chunks. It uses Redis Lists to append chunks atomically and read them all
// at finalization time.
//
// The stream store is also exposed via NewStreamStore for injection into
// UpdatePublisher, so that event publishing can resolve streaming messages
// by reading their buffered chunks.
type StreamStore struct {
	rdb      redis.UniversalClient
	chunkTTL time.Duration
}

// AppendChunk atomically appends a chunk to the Redis list and refreshes
// the TTL using the AppendChunk Lua script from cache.
// This replaces the previous non-atomic RPush + Expire two-command sequence.
func (s *StreamStore) AppendChunk(messageID string, chunk string) (int64, error) {
	key := cache.MessageStream(messageID)
	ttlSeconds := int(s.chunkTTL / time.Second)
	result, err := cache.AppendChunk.Run(
		context.Background(), s.rdb,
		[]string{key},
		ttlSeconds, chunk,
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("append chunk: %w", err)
	}
	return result, nil
}

func (s *StreamStore) GetAllChunks(messageID string) ([]string, error) {
	key := cache.MessageStream(messageID)
	chunks, err := s.rdb.LRange(context.Background(), key, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("get all chunks: %w", err)
	}
	return chunks, nil
}

func (s *StreamStore) DeleteChunks(messageID string) error {
	key := cache.MessageStream(messageID)
	if err := s.rdb.Del(context.Background(), key).Err(); err != nil {
		return fmt.Errorf("delete chunks: %w", err)
	}
	return nil
}

// streamStateMap is a concurrent-safe map of turnID → *turnStreamState.
// It uses sync.Map for lock-free reads in the common case (each turn writes
// to its own key, so contention is minimal).
type streamStateMap struct {
	m sync.Map // map[string]*turnStreamState
}

// getOrCreate returns the existing state for turnID, or creates a new one.
func (m *streamStateMap) getOrCreate(turnID string) *turnStreamState {
	if v, ok := m.m.Load(turnID); ok {
		return v.(*turnStreamState)
	}
	state := &turnStreamState{}
	actual, _ := m.m.LoadOrStore(turnID, state)
	return actual.(*turnStreamState)
}

// remove cleans up the state for a turn.
func (m *streamStateMap) remove(turnID string) {
	m.m.Delete(turnID)
}
