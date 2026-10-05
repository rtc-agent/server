// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// UserUsecase handles user-related operations.
type UserUsecase struct {
	userRepo repo.UserRepo
}

// NewUserUsecase creates a new UserUsecase.
func NewUserUsecase(userRepo repo.UserRepo) *UserUsecase {
	return &UserUsecase{
		userRepo: userRepo,
	}
}

// ListUsersPaginated returns a paginated list of users.
func (uc *UserUsecase) ListUsersPaginated(ctx context.Context, page, pageSize int) ([]*model.User, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	users, total, err := uc.userRepo.List(ctx, page, pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}

	return users, total, nil
}
