package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// =============================================================================
// Stream store helper
// =============================================================================

// NewStreamStore creates a stream store accessor backed by Redis.
// The returned value satisfies updates.StreamStoreAccessor and can be injected
// into UpdatePublisher via SetStreamStore. It uses the same Redis key format
// (cache.MessageStream) as the stream buffering logic in this package.
//
// ttlCheckInterval controls the sampling interval for TTL checks. If <= 0,
// defaults to 100. See StreamStore.ttlCheckInterval for details.
func NewStreamStore(rdb redis.UniversalClient, chunkTTL time.Duration, ttlCheckInterval int) *StreamStore {
	if ttlCheckInterval <= 0 {
		ttlCheckInterval = 100
	}
	return &StreamStore{
		rdb:              rdb,
		chunkTTL:         chunkTTL,
		ttlCheckInterval: ttlCheckInterval,
		ttlCheckCounter:  make(map[string]int),
	}
}

func (h *helpers) getStreamStore(ctx context.Context) *StreamStore {
	// Read stream_chunk_ttl dynamically from ConfigProvider when available.
	chunkTTL := h.streamChunkTTL
	if h.deps != nil && h.deps.ConfigProvider != nil {
		if v, err := h.deps.ConfigProvider.GetEffectiveDuration(ctx, "worker.stream_chunk_ttl", userIDFromContext(ctx)); err == nil && v > 0 {
			chunkTTL = v
		}
	}
	// ttlCheckInterval is static (not dynamically configurable) to avoid
	// per-call overhead. Use the configured value from helpers.
	return &StreamStore{
		rdb:              h.rdb,
		chunkTTL:         chunkTTL,
		ttlCheckInterval: h.streamTTLCheckInterval,
		ttlCheckCounter:  make(map[string]int),
	}
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

	// ttlCheckInterval is the number of AppendChunk calls between TTL checks.
	// Checking at intervals (rather than on every append) reduces Redis load by
	// ~99% for high-throughput streams while still surfacing TTL drift issues
	// within a reasonable timeframe. Default: 100. Configurable via
	// worker.stream_ttl_check_interval for different workload profiles.
	ttlCheckInterval int

	// ttlCheckCounter tracks the number of AppendChunk calls per messageID
	// to implement sampling-based TTL checks. Instead of checking TTL on every
	// append (which adds Redis load), we check every ttlCheckInterval calls.
	// This reduces Redis RTTs by ~99% for high-throughput streams.
	//
	// Memory safety: Entries are cleaned up in DeleteChunks and GetAndDeleteChunks.
	// If cleanup is missed (e.g., due to crashes), entries remain but are bounded
	// by the number of concurrent streaming messages. For long-running processes
	// with many missed cleanups, consider periodic cleanup via a background goroutine.
	//
	// TODO(optimization): Add periodic cleanup for ttlCheckCounter to prevent
	// potential memory leaks in long-running processes with high stream volume.
	// Options:
	//   1. Background goroutine that scans and removes stale entries every hour
	//   2. LRU cache with bounded size (e.g., sync.Map with size limit)
	//   3. Move cleanup to a separate metrics collector that runs periodically
	// Current risk: Low - entries are bounded by concurrent streams, but monitoring
	// is recommended for production deployments with high message throughput.
	ttlCheckCounter map[string]int
	ttlCheckMu      sync.Mutex
}

// streamStoreRedisTimeout is the timeout for Redis operations in StreamStore.
// Stream operations are latency-sensitive (part of the streaming path), so
// the timeout is shorter than checkpoint operations.
const streamStoreRedisTimeout = 5 * time.Second

// maxStreamChunks is the maximum number of chunks to read from Redis in a
// single GetAllChunks call. This prevents blocking Redis with very large lists.
// If a message exceeds this limit, the result is truncated and a warning is logged.
//
// Design rationale: 10000 chunks provides a good balance between:
//   - Preventing Redis blocking from unbounded lists (e.g., runaway streaming)
//   - Supporting large messages (typical messages are 10-100 chunks)
//   - Allowing graceful degradation (truncation + warning) instead of hard failure
//
// This limit is intentionally hardcoded rather than configurable, as it represents
// a safety bound rather than a tunable performance parameter.
const maxStreamChunks = 10000

// AppendChunk atomically appends a chunk to the Redis list and refreshes
// the TTL using the AppendChunk Lua script from cache.
// This replaces the previous non-atomic RPush + Expire two-command sequence.
//
// Uses context.Background() with timeout because stream chunks are buffered in
// Redis as an intermediate step; the caller's context may be cancelled mid-stream,
// but partial chunks already written should remain consistent.
//
// TTL Check Optimization: Instead of checking TTL on every append (which adds
// a Redis RTT per chunk), we perform sampling-based checks every ttlCheckInterval
// calls. This reduces Redis load by ~99% for high-throughput streams while still
// surfacing TTL drift issues within a reasonable timeframe.
func (s *StreamStore) AppendChunk(messageID string, chunk string) (int64, error) {
	key := cache.MessageStream(messageID)
	ttlSeconds := int(s.chunkTTL / time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), streamStoreRedisTimeout)
	defer cancel()

	// Sampling-based TTL check: increment counter and check only at intervals.
	// This reduces Redis load while still detecting TTL drift issues.
	s.ttlCheckMu.Lock()
	s.ttlCheckCounter[messageID]++
	shouldCheck := s.ttlCheckCounter[messageID]%s.ttlCheckInterval == 0
	s.ttlCheckMu.Unlock()

	if shouldCheck {
		// Check remaining TTL periodically. If the key already exists and its
		// TTL has dropped below half of chunkTTL, it indicates the stream has been
		// active for a long time without finalization. Log a warning to surface
		// potential stuck streams or TTL consistency issues.
		if remainingTTL, ttlErr := s.rdb.TTL(ctx, key).Result(); ttlErr == nil && remainingTTL > 0 && remainingTTL < s.chunkTTL/2 {
			logger.Warn(ctx, "stream TTL approaching expiry",
				zap.String("message_id", messageID),
				zap.Duration("remaining_ttl", remainingTTL),
				zap.Duration("chunk_ttl", s.chunkTTL),
				zap.Int("check_interval", s.ttlCheckInterval),
			)
		}
	}

	result, err := cache.AppendChunk.Run(
		ctx, s.rdb,
		[]string{key},
		ttlSeconds, chunk,
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("append chunk: %w", err)
	}
	return result, nil
}

// GetAllChunks reads all buffered chunks for the given message from Redis.
// If the number of chunks exceeds the maximum limit (maxStreamChunks), it
// truncates the result and logs a warning to prevent blocking Redis with
// very large lists.
func (s *StreamStore) GetAllChunks(messageID string) ([]string, error) {
	key := cache.MessageStream(messageID)
	ctx, cancel := context.WithTimeout(context.Background(), streamStoreRedisTimeout)
	defer cancel()

	chunks, err := s.rdb.LRange(ctx, key, 0, int64(maxStreamChunks)-1).Result()
	if err != nil {
		return nil, fmt.Errorf("get all chunks: %w", err)
	}

	// Check if there are more chunks beyond the limit
	if len(chunks) == maxStreamChunks {
		remaining, _ := s.rdb.LLen(ctx, key).Result()
		if remaining > int64(maxStreamChunks) {
			logger.Warn(ctx, "stream chunks truncated",
				zap.String("message_id", messageID),
				zap.Int64("total", remaining),
				zap.Int("returned", maxStreamChunks))
		}
	}

	return chunks, nil
}

// DeleteChunks removes all buffered chunks for the given message from Redis.
// Also cleans up the TTL check counter to prevent memory leaks.
func (s *StreamStore) DeleteChunks(messageID string) error {
	key := cache.MessageStream(messageID)
	ctx, cancel := context.WithTimeout(context.Background(), streamStoreRedisTimeout)
	defer cancel()

	// Clean up TTL check counter to prevent memory leaks in long-running processes
	s.ttlCheckMu.Lock()
	delete(s.ttlCheckCounter, messageID)
	s.ttlCheckMu.Unlock()

	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("delete chunks: %w", err)
	}
	return nil
}

// GetAndDeleteChunks atomically reads all buffered chunks and deletes them from Redis.
// This prevents race conditions where concurrent readers might see partial data between
// GetAllChunks and DeleteChunks operations.
// Also cleans up the TTL check counter to prevent memory leaks.
//
// Returns the list of chunks (empty if no chunks exist).
func (s *StreamStore) GetAndDeleteChunks(messageID string) ([]string, error) {
	key := cache.MessageStream(messageID)
	ctx, cancel := context.WithTimeout(context.Background(), streamStoreRedisTimeout)
	defer cancel()

	// Clean up TTL check counter to prevent memory leaks in long-running processes
	s.ttlCheckMu.Lock()
	delete(s.ttlCheckCounter, messageID)
	s.ttlCheckMu.Unlock()

	result, err := cache.GetAndDelChunks.Run(ctx, s.rdb, []string{key}).Result()
	if err != nil {
		return nil, fmt.Errorf("get and delete chunks: %w", err)
	}

	// Convert result to []string
	if result == nil {
		return []string{}, nil
	}
	chunks, ok := result.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	resultStrs := make([]string, len(chunks))
	for i, chunk := range chunks {
		if s, ok := chunk.(string); ok {
			resultStrs[i] = s
		}
	}

	return resultStrs, nil
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
