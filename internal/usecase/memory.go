// Package usecase provides memory-related use cases.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/memory"
)

// Memory use case errors.
var (
	ErrMemoryNotConfigured = errors.New("memory repository not configured")
	ErrForbidden           = errors.New("forbidden: not authorized")
	ErrSessionNotFound     = errors.New("session not found")
)

// MemoryUsecase handles memory export operations.
type MemoryUsecase struct {
	memoryRepo  memory.Repository
	sessionRepo memorySessionRepo // Interface to avoid circular dependency
}

// memorySessionRepo is a minimal interface for session operations needed by MemoryUsecase.
type memorySessionRepo interface {
	GetByID(ctx context.Context, id uuid.UUID) (*model.Session, error)
}

// NewMemoryUsecase creates a new MemoryUsecase.
func NewMemoryUsecase(memoryRepo memory.Repository, sessionRepo memorySessionRepo) *MemoryUsecase {
	return &MemoryUsecase{
		memoryRepo:  memoryRepo,
		sessionRepo: sessionRepo,
	}
}

// ExportMemories exports memories in OKF bundle format.
// Validates ownership before exporting.
func (uc *MemoryUsecase) ExportMemories(ctx context.Context, userID uuid.UUID, scope memory.ScopeType, scopeID uuid.UUID, opts memory.ExportOptions, w io.Writer) error {
	// Check if memory repo is configured
	if uc.memoryRepo == nil {
		return ErrMemoryNotConfigured
	}

	// Validate scope ownership
	switch scope {
	case memory.ScopeUser:
		// User scope: caller may only export their own memories
		if scopeID != userID {
			return ErrForbidden
		}
	case memory.ScopeSession:
		// Session scope: caller must own the session
		session, err := uc.sessionRepo.GetByID(ctx, scopeID)
		if err != nil {
			return ErrSessionNotFound
		}
		if session.OwnerRefID != userID.String() {
			return ErrForbidden
		}
	case memory.ScopeGlobal:
		// Global scope: no ownership check
	}

	// Create exporter and run export
	exporter := memory.NewExporter(uc.memoryRepo)
	if err := exporter.Export(ctx, opts, w); err != nil {
		return fmt.Errorf("export memories: %w", err)
	}

	return nil
}

// IsMemoryConfigured checks if memory repository is configured.
func (uc *MemoryUsecase) IsMemoryConfigured() bool {
	return uc.memoryRepo != nil
}
