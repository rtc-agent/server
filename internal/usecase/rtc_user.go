// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

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
	oauth2UserRepo   repo.OAuth2UserRepo
	refreshTokenRepo repo.RefreshTokenRepo
	banPublisher     BanPublisher
}

// NewRtcUserUsecase creates a new RtcUserUsecase.
func NewRtcUserUsecase(
	oauth2UserRepo repo.OAuth2UserRepo,
	refreshTokenRepo repo.RefreshTokenRepo,
) *RtcUserUsecase {
	return &RtcUserUsecase{
		oauth2UserRepo:   oauth2UserRepo,
		refreshTokenRepo: refreshTokenRepo,
	}
}

// SetBanPublisher injects the ban publisher for distributed sync.
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
func (uc *RtcUserUsecase) BanUser(ctx context.Context, input BanUserInput) (*model.OAuth2User, error) {
	// 1. Update user status
	user, err := uc.oauth2UserRepo.FindByID(ctx, input.UserID)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}

	now := time.Now()
	user.BannedAt = &now
	user.BannedReason = input.Reason

	if err := uc.oauth2UserRepo.Update(ctx, user); err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}

	// 2. Revoke all refresh tokens
	revokedCount, err := uc.refreshTokenRepo.RevokeAllByUserID(ctx, input.UserID)
	if err != nil {
		logger.Error(ctx, "ban_user.revoke_tokens_failed",
			zap.String("user_id", input.UserID.String()),
			zap.Error(err))
		// Continue even if token revocation fails - the ban is already in effect
	} else {
		logger.Info(ctx, "ban_user.tokens_revoked",
			zap.String("user_id", input.UserID.String()),
			zap.Int64("revoked_count", revokedCount))
	}

	// 3. Publish ban event for distributed sync
	if uc.banPublisher != nil {
		if err := uc.banPublisher.PublishBan(ctx, input.UserID, "ban", input.Reason); err != nil {
			logger.Error(ctx, "ban_user.publish_failed",
				zap.String("user_id", input.UserID.String()),
				zap.Error(err))
			// Continue even if publish fails - the ban is already in effect
		}
	}

	logger.Info(ctx, "ban_user.success",
		zap.String("user_id", input.UserID.String()),
		zap.String("reason", input.Reason))

	return user, nil
}

// UnbanUser unbans a user and publishes an unban event.
func (uc *RtcUserUsecase) UnbanUser(ctx context.Context, userID uuid.UUID) (*model.OAuth2User, error) {
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

	// 2. Publish unban event for distributed sync
	if uc.banPublisher != nil {
		if err := uc.banPublisher.PublishBan(ctx, userID, "unban", ""); err != nil {
			logger.Error(ctx, "unban_user.publish_failed",
				zap.String("user_id", userID.String()),
				zap.Error(err))
			// Continue even if publish fails - the unban is already in effect
		}
	}

	logger.Info(ctx, "unban_user.success",
		zap.String("user_id", userID.String()))

	return user, nil
}
