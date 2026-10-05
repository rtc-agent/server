// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// AdminUserUsecase handles admin user-related operations.
type AdminUserUsecase struct {
	adminUserRepo repo.AdminUserRepo
}

// NewAdminUserUsecase creates a new AdminUserUsecase.
func NewAdminUserUsecase(adminUserRepo repo.AdminUserRepo) *AdminUserUsecase {
	return &AdminUserUsecase{
		adminUserRepo: adminUserRepo,
	}
}

// ListUsersPaginated returns a paginated list of admin users.
func (uc *AdminUserUsecase) ListUsersPaginated(ctx context.Context, page, pageSize int) ([]*model.AdminUser, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	users, total, err := uc.adminUserRepo.List(ctx, page, pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("list admin users: %w", err)
	}

	return users, total, nil
}
