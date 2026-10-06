// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
)

// BanPublisher publishes ban/unban events for distributed synchronization.
type BanPublisher interface {
	PublishBan(ctx context.Context, userID uuid.UUID, action, reason string) error
}

// RtcUserUsecase handles RTC user management operations.
type RtcUserUsecase struct {
	db               *gorm.DB
	oauth2UserRepo   repo.OAuth2UserRepo
	refreshTokenRepo repo.RefreshTokenRepo
	banPublisher     BanPublisher
	auditLogRepo     repo.AuditLogRepo
}

// NewRtcUserUsecase creates a new RtcUserUsecase.
func NewRtcUserUsecase(
	db *gorm.DB,
	oauth2UserRepo repo.OAuth2UserRepo,
	refreshTokenRepo repo.RefreshTokenRepo,
	auditLogRepo repo.AuditLogRepo,
) *RtcUserUsecase {
	return &RtcUserUsecase{
		db:               db,
		oauth2UserRepo:   oauth2UserRepo,
		refreshTokenRepo: refreshTokenRepo,
		auditLogRepo:     auditLogRepo,
	}
}

// SetBanPublisher injects the ban publisher for distributed sync.
// SetBanPublisher must be called before any BanUser/UnbanUser calls.
func (uc *RtcUserUsecase) SetBanPublisher(publisher BanPublisher) {
	uc.banPublisher = publisher
}

// ListUsersFilter holds filter criteria for listing users.
type ListUsersFilter struct {
	Status   string // "active" or "banned"
	Search   string // search in email and name
	Page     int
	PageSize int
}

// ListUsersResult holds the result of listing users.
type ListUsersResult struct {
	Users    []*model.OAuth2User
	Total    int64
	Page     int
	PageSize int
}

// ListUsers returns a paginated list of users with optional filters.
func (uc *RtcUserUsecase) ListUsers(ctx context.Context, filter ListUsersFilter) (*ListUsersResult, error) {
	repoFilter := repo.OAuth2UserFilter{
		Status:   filter.Status,
		Search:   filter.Search,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}

	users, total, err := uc.oauth2UserRepo.ListWithFilters(ctx, repoFilter)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	return &ListUsersResult{
		Users:    users,
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}, nil
}

// GetUser returns a user by ID.
func (uc *RtcUserUsecase) GetUser(ctx context.Context, userID uuid.UUID) (*model.OAuth2User, error) {
	user, err := uc.oauth2UserRepo.FindByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}

// BanUserInput holds the input for banning a user.
type BanUserInput struct {
	UserID uuid.UUID
	Reason string
}

// BanUser bans a user, revokes all refresh tokens, and publishes a ban event.
func (uc *RtcUserUsecase) BanUser(ctx context.Context, input BanUserInput, adminUserID uuid.UUID, adminIP string) (*model.OAuth2User, error) {
	// 1. Find user first
	user, err := uc.oauth2UserRepo.FindByID(ctx, input.UserID)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}

	now := time.Now()
	user.BannedAt = &now
	user.BannedReason = input.Reason

	// 2. Update user + revoke tokens in a transaction
	var revokedCount int64
	if err := uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		if err := uc.oauth2UserRepo.Update(txCtx, user); err != nil {
			return fmt.Errorf("update user: %w", err)
		}

		var revokeErr error
		revokedCount, revokeErr = uc.refreshTokenRepo.RevokeAllByUserID(txCtx, input.UserID)
		if revokeErr != nil {
			// Rollback transaction - token revocation is critical for security
			return fmt.Errorf("revoke tokens: %w", revokeErr)
		}
		logger.Info(txCtx, "ban_user.tokens_revoked",
			zap.String("user_id", input.UserID.String()),
			zap.Int64("revoked_count", revokedCount))

		return nil
	}); err != nil {
		return nil, fmt.Errorf("ban user transaction: %w", err)
	}

	// 2.5 Update ban cache immediately after transaction succeeds
	// This ensures all instances see the banned status without waiting for cache miss + DB query
	middleware.SetBanCache(ctx, input.UserID, true)

	// 3. Publish ban event for distributed sync (outside transaction)
	if uc.banPublisher != nil {
		if err := uc.banPublisher.PublishBan(ctx, input.UserID, "ban", input.Reason); err != nil {
			logger.Error(ctx, "ban_user.publish_failed",
				zap.String("user_id", input.UserID.String()),
				zap.Error(err))
			// Continue even if publish fails - the ban is already in effect
		}
	}

	// 4. Write audit log
	if uc.auditLogRepo != nil {
		if err := uc.auditLogRepo.Create(ctx, repo.NewAuditLog(
			adminUserID, adminIP, "ban_user", "rtc_user", input.UserID,
			map[string]any{"reason": input.Reason, "revoked_tokens": revokedCount},
		)); err != nil {
			logger.Error(ctx, "ban_user.audit_log_failed", zap.Error(err))
		}
	}

	logger.Info(ctx, "ban_user.success",
		zap.String("user_id", input.UserID.String()),
		zap.String("reason", input.Reason),
		zap.String("admin_user_id", adminUserID.String()))

	return user, nil
}

// UnbanUser unbans a user and publishes an unban event.
func (uc *RtcUserUsecase) UnbanUser(ctx context.Context, userID uuid.UUID, adminUserID uuid.UUID, adminIP string) (*model.OAuth2User, error) {
	// 1. Update user status
	user, err := uc.oauth2UserRepo.FindByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}

	user.BannedAt = nil
	user.BannedReason = ""

	if err := uc.oauth2UserRepo.Update(ctx, user); err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}

	// 2. Invalidate ban cache to ensure immediate effect
	middleware.InvalidateBanCache(ctx, userID)

	// 3. Publish unban event for distributed sync
	if uc.banPublisher != nil {
		if err := uc.banPublisher.PublishBan(ctx, userID, "unban", ""); err != nil {
			logger.Error(ctx, "unban_user.publish_failed",
				zap.String("user_id", userID.String()),
				zap.Error(err))
			// Continue even if publish fails - the unban is already in effect
		}
	}

	// 4. Write audit log
	if uc.auditLogRepo != nil {
		if err := uc.auditLogRepo.Create(ctx, repo.NewAuditLog(
			adminUserID, adminIP, "unban_user", "rtc_user", userID, nil,
		)); err != nil {
			logger.Error(ctx, "unban_user.audit_log_failed", zap.Error(err))
		}
	}

	logger.Info(ctx, "unban_user.success",
		zap.String("user_id", userID.String()),
		zap.String("admin_user_id", adminUserID.String()))

	return user, nil
}
