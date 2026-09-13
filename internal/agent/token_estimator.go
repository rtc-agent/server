package agent

import (
	"context"
	"math"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/repo"
)

// TokenEstimate Token 预估结果。
// 表示一个 session 的当前 token 使用情况和对未来的预测。
type TokenEstimate struct {
	// CurrentTokens 当前上下文 token 数（= Session.TotalTokens，含所有类型）
	CurrentTokens int64

	// EstimatedNextRound 预估下一轮 token 数
	EstimatedNextRound int64

	// CompressionThreshold 压缩阈值
	CompressionThreshold int64

	// CompressionProgress 压缩进度（0-100）
	CompressionProgress float64

	// RoundsUntilCompression 距离压缩的轮次
	// 0 = 下一轮触发
	// -1 = 已超过阈值
	RoundsUntilCompression int

	// NewEWMA 计算后的新 EWMA 值，调用方应通过 AtomicAddTokenUsage.SetEWMA 持久化。
	NewEWMA float64
}

// TokenEstimator Token 预估器。
// 使用 EWMA（指数加权移动平均）增量预估下一轮 token 使用量。
// EWMA 持久化在 Session.TokenEstimateEWMA 字段中。
type TokenEstimator struct {
	sessionRepo      repo.SessionRepo
	triggerThreshold int64   // 实际触发阈值（contextLimit - compactBuffer）
	alpha            float64 // EWMA 衰减因子
	defaultGrowth    float64 // 默认初始增长率
}

// NewTokenEstimator 创建 Token 预估器。
//   - contextLimit: 模型上下文窗口大小
//   - compactBuffer: 压缩缓冲 token 数
//   - sessionRepo: Session 仓库（用于读写 EWMA）
func NewTokenEstimator(contextLimit, compactBuffer int, sessionRepo repo.SessionRepo) *TokenEstimator {
	threshold := int64(contextLimit - compactBuffer)
	if threshold <= 0 {
		threshold = 1
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

// Estimate 计算 Token 预估并返回新 EWMA（不持久化，调用方通过 AtomicAddTokenUsage.SetEWMA 持久化）。
//
// Parameters:
//   - sessionID: 会话 ID
//   - sessionTotalTokens: 当前 Session.TotalTokens（含本轮 delta）
//   - prevEWMA: 上一次的 EWMA（从 Session.TokenEstimateEWMA 读取）
//   - roundDelta: 本轮的 token 增量（= usage.TotalTokens）
//
// Returns:
//   - *TokenEstimate: 预估结果（含 NewEWMA 供调用方持久化）
func (e *TokenEstimator) Estimate(
	_ context.Context,
	_ uuid.UUID,
	sessionTotalTokens int64,
	prevEWMA float64,
	roundDelta int64,
) *TokenEstimate {
	// 1. 使用传入的 EWMA（来自 Session.TokenEstimateEWMA）
	lastEWMA := prevEWMA
	if lastEWMA <= 0 {
		lastEWMA = e.defaultGrowth
	}

	// 2. EWMA 增量更新：α * old_ewma + (1-α) * new_observation
	newEWMA := e.alpha*lastEWMA + (1-e.alpha)*float64(roundDelta)

	// 3. 预估下一轮
	estimatedNext := sessionTotalTokens + int64(newEWMA)

	// 4. 计算压缩进度
	progress := float64(sessionTotalTokens) / float64(e.triggerThreshold) * 100
	progress = math.Min(progress, 100)

	// 5. 计算距离压缩的轮次
	roundsUntil := calcRounds(sessionTotalTokens, e.triggerThreshold, newEWMA)

	return &TokenEstimate{
		CurrentTokens:          sessionTotalTokens,
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

	currentTokens := session.TotalTokens
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

// calcRounds calculates the estimated rounds until compression threshold is reached.
func calcRounds(currentTokens, threshold int64, ewma float64) int {
	if currentTokens >= threshold {
		return -1
	}
	if ewma > 0 {
		remaining := threshold - currentTokens
		return int(float64(remaining) / ewma)
	}
	return 999
}
