package agent

import (
	"context"
	"math"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/repo"
)

// TokenEstimate holds the token estimation result.
// Represents a session's current token usage and prediction for the future.
type TokenEstimate struct {
	// CurrentTokens is the current context token count (= Session.TotalTokens, all types).
	CurrentTokens int64

	// EstimatedNextRound is the estimated token count for the next round.
	EstimatedNextRound int64

	// CompressionThreshold is the threshold at which compression is triggered.
	CompressionThreshold int64

	// CompressionProgress is the compression progress (0-100).
	CompressionProgress float64

	// RoundsUntilCompression is the estimated rounds until compression.
	// 0 = triggered on next round
	// -1 = already exceeded threshold
	RoundsUntilCompression int

	// NewEWMA is the newly computed EWMA value. Caller should persist via AtomicAddTokenUsage.SetEWMA.
	NewEWMA float64
}

// TokenEstimator estimates token usage incrementally.
// Uses EWMA (Exponentially Weighted Moving Average) to predict the next round's token usage.
// EWMA is persisted in the Session.TokenEstimateEWMA field.
type TokenEstimator struct {
	sessionRepo      repo.SessionRepo
	triggerThreshold int64   // actual trigger threshold (contextLimit - compactBuffer)
	alpha            float64 // EWMA decay factor
	defaultGrowth    float64 // default initial growth rate
}

// NewTokenEstimator creates a TokenEstimator.
//   - contextLimit: model context window size
//   - compactBuffer: compression buffer token count
//   - sessionRepo: Session repository (for reading/writing EWMA)
func NewTokenEstimator(contextLimit, compactBuffer int, sessionRepo repo.SessionRepo) *TokenEstimator {
	threshold := int64(contextLimit - compactBuffer)
	if threshold <= 0 {
		// Fallback to 80% of contextLimit when configuration is invalid
		// This prevents threshold=1 which would cause compression on every turn
		threshold = int64(float64(contextLimit) * 0.8)
		// Note: Fallback logging is handled by summarize.go's "summarize.threshold_fallback"
		// and servicecontext.go's "servicecontext.threshold_fallback" warnings.
	}
	return &TokenEstimator{
		sessionRepo:      sessionRepo,
		triggerThreshold: threshold,
		alpha:            0.3,
		defaultGrowth:    2000.0,
	}
}

// TriggerThreshold returns the compression trigger threshold.
func (e *TokenEstimator) TriggerThreshold() int64 {
	return e.triggerThreshold
}

// Estimate computes the token estimate and returns the new EWMA (not persisted; caller persists via AtomicAddTokenUsage.SetEWMA).
//
// Parameters:
//   - sessionID: session ID
//   - currentContextTokens: current context token count (including this round's delta), used for progress and rounds calculation
//   - prevEWMA: previous EWMA (read from Session.TokenEstimateEWMA)
//   - roundDelta: this round's token increment (= usage.TotalTokens)
//
// Returns:
//   - *TokenEstimate: estimation result (includes NewEWMA for caller to persist)
func (e *TokenEstimator) Estimate(
	_ context.Context,
	_ uuid.UUID,
	currentContextTokens int64,
	prevEWMA float64,
	roundDelta int64,
) *TokenEstimate {
	// 1. Use the provided EWMA (from Session.TokenEstimateEWMA).
	// When prevEWMA <= 0 (first call or after reset), use defaultGrowth as a
	// reasonable fallback. This is defensive programming, not a bug.
	// prevEWMA itself is not modified; only the local lastEWMA gets the fallback.
	lastEWMA := prevEWMA
	if lastEWMA <= 0 {
		lastEWMA = e.defaultGrowth
	}

	// 2. EWMA incremental update: alpha * old_ewma + (1-alpha) * new_observation
	newEWMA := e.alpha*lastEWMA + (1-e.alpha)*float64(roundDelta)

	// 3. Estimate next round (based on current context size + increment).
	estimatedNext := currentContextTokens + int64(newEWMA)

	// 4. Calculate compression progress.
	progress := float64(currentContextTokens) / float64(e.triggerThreshold) * 100
	progress = math.Min(progress, 100)

	// 5. Calculate rounds until compression.
	roundsUntil := calcRounds(currentContextTokens, e.triggerThreshold, newEWMA)

	return &TokenEstimate{
		CurrentTokens:          currentContextTokens,
		EstimatedNextRound:     estimatedNext,
		CompressionThreshold:   e.triggerThreshold,
		CompressionProgress:    progress,
		RoundsUntilCompression: roundsUntil,
		NewEWMA:                newEWMA,
	}
}

// ReadEWMA reads the current EWMA value from Session.TokenEstimateEWMA.
// Returns 0 if the session is not found.
func (e *TokenEstimator) ReadEWMA(ctx context.Context, sessionID uuid.UUID) float64 {
	session, err := e.sessionRepo.GetByID(ctx, sessionID)
	if err != nil || session == nil {
		return 0
	}
	return session.TokenEstimateEWMA
}

// ReestimateAfterCompact computes a fresh EWMA after context compression
// and persists it to Session.TokenEstimateEWMA.
//
// After compression, the old EWMA (based on pre-compression growth rate) no longer
// applies to the compressed context. This method adjusts the EWMA using the
// compression ratio (tokensAfter/tokensBefore) to preserve useful information.
//
// Parameters:
//   - prevEWMA: the EWMA value saved BEFORE compressContext ran.
//   - tokensBefore/tokensAfter: message token counts before/after compression.
func (e *TokenEstimator) ReestimateAfterCompact(
	ctx context.Context,
	sessionID uuid.UUID,
	prevEWMA float64,
	tokensBefore, tokensAfter int,
) (*TokenEstimate, error) {
	// 1. Compute the new EWMA adjusted by compression ratio.
	var newEWMA float64
	if prevEWMA > 0 && tokensBefore > 0 {
		ratio := float64(tokensAfter) / float64(tokensBefore)
		if ratio > 1.0 {
			ratio = 1.0 // cap: summary longer than original
		}
		newEWMA = prevEWMA * ratio
	} else {
		newEWMA = e.defaultGrowth
	}
	if newEWMA <= 0 {
		newEWMA = e.defaultGrowth
	}

	// 2. Persist the new EWMA to Session table.
	if err := e.sessionRepo.AtomicUpdateEWMA(ctx, sessionID, newEWMA); err != nil {
		return nil, err
	}

	// 3. Read the updated session to get current TotalTokens.
	session, err := e.sessionRepo.GetByID(ctx, sessionID)
	if err != nil || session == nil {
		// Best-effort estimate without persisted session data.
		return &TokenEstimate{
			CurrentTokens:          int64(tokensAfter),
			EstimatedNextRound:     int64(float64(tokensAfter) + newEWMA),
			CompressionThreshold:   e.triggerThreshold,
			CompressionProgress:    math.Min(float64(tokensAfter)/float64(e.triggerThreshold)*100, 100),
			RoundsUntilCompression: calcRounds(int64(tokensAfter), e.triggerThreshold, newEWMA),
			NewEWMA:                newEWMA,
		}, nil
	}

	// Use tokensAfter (actual compressed context size) instead of session.TotalTokens (cumulative value).
	currentTokens := int64(tokensAfter)
	estimatedNext := currentTokens + int64(newEWMA)
	progress := math.Min(float64(currentTokens)/float64(e.triggerThreshold)*100, 100)
	roundsUntil := calcRounds(currentTokens, e.triggerThreshold, newEWMA)

	return &TokenEstimate{
		CurrentTokens:          currentTokens,
		EstimatedNextRound:     estimatedNext,
		CompressionThreshold:   e.triggerThreshold,
		CompressionProgress:    progress,
		RoundsUntilCompression: roundsUntil,
		NewEWMA:                newEWMA,
	}, nil
}

// roundsUnknown is the sentinel value returned when the estimated rounds
// until compression is effectively unknown (EWMA <= 0).
const roundsUnknown = 999

// calcRounds calculates the estimated rounds until compression threshold is reached.
// Note: When ewma is very small (e.g., after multiple rounds with roundDelta=0),
// the EWMA naturally decays. This is normal EWMA behavior, not a bug.
// The explicit guard (if ewma > 0) prevents division by zero; when ewma <= 0
// we return roundsUnknown as a sentinel meaning "effectively unknown / very large".
func calcRounds(currentTokens, threshold int64, ewma float64) int {
	if currentTokens >= threshold {
		return -1
	}
	if ewma > 0 {
		remaining := threshold - currentTokens
		return int(float64(remaining) / ewma)
	}
	return roundsUnknown
}
