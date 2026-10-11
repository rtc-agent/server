// batch_token_updater.go implements a batch update mechanism for token usage.
//
// Problem: Every LLM call triggers an immediate DB update via AtomicAddTokenUsage.
// In a ReAct loop with 5-10 LLM calls per turn, this means 5-10 DB round-trips,
// each updating the same session row. This creates unnecessary DB load and latency.
//
// Solution: Accumulate token updates in memory and flush them periodically:
//   - Time-based: flush every N seconds (default: 5s)
//   - Count-based: flush after M updates (default: 10)
//   - Whichever comes first
//
// Benefits:
//   - Reduces DB load by 5-10x during active turns
//   - Lower latency for LLM calls (no immediate DB write)
//   - Still provides reasonable durability (5s window)
//
// Trade-offs:
//   - Up to 5s of token data may be lost on crash (acceptable for metrics)
//   - Requires careful synchronization to prevent race conditions
//   - Must flush on session close to avoid losing final updates
//
// Thread safety: All operations are protected by sync.Mutex to prevent races
// between concurrent LLM calls updating the same session.

package agent

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	dbmodel "github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// BatchTokenUpdater accumulates token updates in memory and flushes them to the
// database periodically, reducing DB load during active turns.
type BatchTokenUpdater struct {
	sessionRepo repo.SessionRepo
	logger      turnagent.Logger

	// Configuration
	flushInterval time.Duration // flush every N seconds
	flushCount    int           // flush after M updates per session

	// State (protected by mu)
	mu       sync.Mutex
	updates  map[uuid.UUID]*pendingUpdate // pending updates per session
	timers   map[uuid.UUID]*time.Timer    // flush timers per session
	shutdown chan struct{}                // shutdown signal
}

// pendingUpdate holds accumulated token updates for a single session.
type pendingUpdate struct {
	sessionID   uuid.UUID
	delta       dbmodel.TokenUsageDelta
	updateCount int       // number of LLM calls accumulated
	firstUpdate time.Time // time of first update (for metrics)
	lastUpdate  time.Time // time of last update (for metrics)
}

// NewBatchTokenUpdater creates a new BatchTokenUpdater.
//
// Parameters:
//   - sessionRepo: the session repository for flushing updates
//   - logger: logger for warnings and errors
//   - flushInterval: flush every N seconds (0 = use default 5s)
//   - flushCount: flush after M updates per session (0 = use default 10)
func NewBatchTokenUpdater(
	sessionRepo repo.SessionRepo,
	log turnagent.Logger,
	flushInterval time.Duration,
	flushCount int,
) *BatchTokenUpdater {
	if flushInterval <= 0 {
		flushInterval = 5 * time.Second
	}
	if flushCount <= 0 {
		flushCount = 10
	}

	return &BatchTokenUpdater{
		sessionRepo:   sessionRepo,
		logger:        log,
		flushInterval: flushInterval,
		flushCount:    flushCount,
		updates:       make(map[uuid.UUID]*pendingUpdate),
		timers:        make(map[uuid.UUID]*time.Timer),
		shutdown:      make(chan struct{}),
	}
}

// Accumulate adds a token update to the pending queue for the given session.
// If the session has accumulated enough updates (>= flushCount), it triggers
// an immediate flush. Otherwise, it schedules a flush after flushInterval.
//
// This method is thread-safe and can be called from multiple goroutines.
func (b *BatchTokenUpdater) Accumulate(ctx context.Context, sessionID uuid.UUID, delta dbmodel.TokenUsageDelta) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Check if we're shutting down
	select {
	case <-b.shutdown:
		// Shutdown in progress, flush immediately (blocking)
		return b.flushSession(ctx, sessionID)
	default:
	}

	// Get or create pending update for this session
	pending, exists := b.updates[sessionID]
	if !exists {
		pending = &pendingUpdate{
			sessionID:   sessionID,
			delta:       delta,
			updateCount: 1,
			firstUpdate: time.Now(),
			lastUpdate:  time.Now(),
		}
		b.updates[sessionID] = pending
	} else {
		// Accumulate deltas
		pending.delta.InputDelta += delta.InputDelta
		pending.delta.OutputDelta += delta.OutputDelta
		pending.delta.TotalDelta += delta.TotalDelta
		pending.delta.CachedReadDelta += delta.CachedReadDelta
		pending.delta.CachedWriteDelta += delta.CachedWriteDelta
		pending.delta.ReasoningDelta += delta.ReasoningDelta
		pending.delta.CostMicrosDelta += delta.CostMicrosDelta

		// For SetEWMA and SetCurrentContextTokens, take the latest value (if > 0)
		if delta.SetEWMA > 0 {
			pending.delta.SetEWMA = delta.SetEWMA
		}
		if delta.SetCurrentContextTokens > 0 {
			pending.delta.SetCurrentContextTokens = delta.SetCurrentContextTokens
		}

		pending.updateCount++
		pending.lastUpdate = time.Now()
	}

	// Check if we should flush immediately
	if pending.updateCount >= b.flushCount {
		return b.flushSessionLocked(ctx, sessionID)
	}

	// Schedule a flush after flushInterval (if not already scheduled)
	if _, timerExists := b.timers[sessionID]; !timerExists {
		timer := time.AfterFunc(b.flushInterval, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			// Remove timer from map (it's firing now)
			delete(b.timers, sessionID)
			// Flush the session (non-blocking context to avoid cancellation issues)
			flushCtx := context.Background()
			if err := b.flushSessionLocked(flushCtx, sessionID); err != nil {
				b.logger.Warn(flushCtx, "batch_token_updater.scheduled_flush_failed", map[string]any{
					"session_id": sessionID,
					"error":      err.Error(),
				})
			}
		})
		b.timers[sessionID] = timer
	}

	return nil
}

// FlushSession immediately flushes all pending updates for the given session.
// This should be called when a session is closed or when immediate durability is required.
//
// This method is thread-safe and can be called from multiple goroutines.
func (b *BatchTokenUpdater) FlushSession(ctx context.Context, sessionID uuid.UUID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushSessionLocked(ctx, sessionID)
}

// flushSessionLocked flushes pending updates for a session.
// Caller must hold b.mu.
func (b *BatchTokenUpdater) flushSessionLocked(ctx context.Context, sessionID uuid.UUID) error {
	pending, exists := b.updates[sessionID]
	if !exists {
		return nil // nothing to flush
	}

	// Cancel the scheduled timer (if any)
	if timer, timerExists := b.timers[sessionID]; timerExists {
		timer.Stop()
		delete(b.timers, sessionID)
	}

	// Remove from pending map before flushing (prevents double-flush on error)
	delete(b.updates, sessionID)

	// Flush to database
	if err := b.sessionRepo.AtomicAddTokenUsage(ctx, sessionID, pending.delta); err != nil {
		b.logger.Warn(ctx, "batch_token_updater.flush_failed", map[string]any{
			"session_id":   sessionID,
			"update_count": pending.updateCount,
			"error":        err.Error(),
		})
		return err
	}

	b.logger.Debug(ctx, "batch_token_updater.flushed", map[string]any{
		"session_id":   sessionID,
		"update_count": pending.updateCount,
		"duration_ms":  pending.lastUpdate.Sub(pending.firstUpdate).Milliseconds(),
		"total_delta":  pending.delta.TotalDelta,
		"cost_micros":  pending.delta.CostMicrosDelta,
	})

	return nil
}

// flushSession flushes pending updates for a session (convenience wrapper).
// Acquires the lock internally.
func (b *BatchTokenUpdater) flushSession(ctx context.Context, sessionID uuid.UUID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushSessionLocked(ctx, sessionID)
}

// Shutdown gracefully shuts down the batch updater, flushing all pending updates.
// This should be called during application shutdown to prevent data loss.
//
// Blocks until all pending updates are flushed or context is cancelled.
func (b *BatchTokenUpdater) Shutdown(ctx context.Context) error {
	close(b.shutdown)

	b.mu.Lock()
	defer b.mu.Unlock()

	// Flush all pending sessions
	for sessionID := range b.updates {
		if err := b.flushSessionLocked(ctx, sessionID); err != nil {
			b.logger.Warn(ctx, "batch_token_updater.shutdown_flush_failed", map[string]any{
				"session_id": sessionID,
				"error":      err.Error(),
			})
			// Continue flushing other sessions even if one fails
		}
	}

	return nil
}

// Stats returns statistics about the batch updater's current state.
// Useful for monitoring and debugging.
func (b *BatchTokenUpdater) Stats() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return map[string]int{
		"pending_sessions": len(b.updates),
		"active_timers":    len(b.timers),
	}
}
