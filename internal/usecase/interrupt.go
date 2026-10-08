// Package usecase provides interrupt-related use cases.
package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
)

// Interrupt use case errors.
var (
	ErrInterruptSessionNotFound = errors.New("session not found")
	ErrInterruptForbidden       = errors.New("forbidden: not authorized for this session")
	ErrInterruptStoreFailed     = errors.New("failed to store interrupt answer")
)

// interruptSessionRepo is a minimal interface for session operations needed by InterruptUsecase.
type interruptSessionRepo interface {
	GetByID(ctx context.Context, id uuid.UUID) (*model.Session, error)
}

// InterruptUsecase handles interrupt answer submission.
type InterruptUsecase struct {
	redis       redis.UniversalClient
	workerCfg   config.WorkerConfig
	sessionRepo interruptSessionRepo
}

// NewInterruptUsecase creates a new InterruptUsecase.
func NewInterruptUsecase(redis redis.UniversalClient, workerCfg config.WorkerConfig, sessionRepo interruptSessionRepo) *InterruptUsecase {
	return &InterruptUsecase{
		redis:       redis,
		workerCfg:   workerCfg,
		sessionRepo: sessionRepo,
	}
}

// SubmitAnswer submits an interrupt answer after validating session ownership.
func (uc *InterruptUsecase) SubmitAnswer(ctx context.Context, userID uuid.UUID, sessionID uuid.UUID, interruptID string, answer string) error {
	// Verify session ownership
	session, err := uc.sessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrInterruptSessionNotFound
	}

	if session.OwnerRefID != userID.String() {
		return ErrInterruptForbidden
	}

	// Atomically SET + PUBLISH (Lua script ensures consistency):
	// 1. SET answer with TTL (catches early arrivals before subscriber is ready)
	// 2. PUBLISH to notify the waiting subscriber
	answerKey := cache.InterruptAnswer(sessionID.String(), interruptID)
	channel := cache.InterruptChannel(sessionID.String(), interruptID)
	ttlSeconds := int(uc.workerCfg.InterruptAnswerTTL.Seconds())

	if err := cache.InterruptSetPublish.Run(ctx, uc.redis,
		[]string{answerKey, channel},
		answer, ttlSeconds,
	).Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrInterruptStoreFailed, err)
	}

	return nil
}
