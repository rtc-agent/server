package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"gorm.io/gorm"
)

// TemporaryCredentialRepo provides temporary credential persistence operations.
type TemporaryCredentialRepo interface {
	// Create stores a new temporary credential record.
	Create(ctx context.Context, cred *model.TemporaryCredential) error
	// GetByAccessKeyID looks up a credential by access_key_id.
	GetByAccessKeyID(ctx context.Context, accessKeyID string) (*model.TemporaryCredential, error)
	// FindExpired finds credentials that expired before the given time.
	FindExpired(ctx context.Context, before time.Time) ([]*model.TemporaryCredential, error)
	// FindByUserID finds all credentials belonging to the given user.
	FindByUserID(ctx context.Context, userID uuid.UUID) ([]*model.TemporaryCredential, error)
	// Delete deletes a credential record.
	Delete(ctx context.Context, id uuid.UUID) error
	// DeleteExpired deletes credentials that expired before the given time.
	// Returns the number of deleted records.
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
	// DeleteByUserID deletes all credentials belonging to the given user.
	// Returns the number of deleted records.
	DeleteByUserID(ctx context.Context, userID uuid.UUID) (int64, error)
	// MarkRevoked sets the revoked flag on a credential (soft delete).
	// P1-4 fix: Used when cache invalidation fails to ensure revoked credentials
	// are rejected even if the cache still contains them.
	MarkRevoked(ctx context.Context, id uuid.UUID) error
}

type temporaryCredentialRepo struct {
	db *gorm.DB
}

// NewTemporaryCredentialRepo creates a new TemporaryCredentialRepo.
func NewTemporaryCredentialRepo(db *gorm.DB) TemporaryCredentialRepo {
	return &temporaryCredentialRepo{db: db}
}

func (r *temporaryCredentialRepo) Create(ctx context.Context, cred *model.TemporaryCredential) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(cred).Error; err != nil {
		return fmt.Errorf("create temporary credential %s: %w", cred.ID, err)
	}
	return nil
}

func (r *temporaryCredentialRepo) GetByAccessKeyID(ctx context.Context, accessKeyID string) (*model.TemporaryCredential, error) {
	var cred model.TemporaryCredential
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("access_key_id = ?", accessKeyID).First(&cred).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get temporary credential by access_key_id %s: %w", accessKeyID, err)
	}
	return &cred, nil
}

func (r *temporaryCredentialRepo) FindExpired(ctx context.Context, before time.Time) ([]*model.TemporaryCredential, error) {
	var creds []*model.TemporaryCredential
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("expires_at < ?", before).
		Find(&creds).Error; err != nil {
		return nil, fmt.Errorf("find expired temporary credentials before %v: %w", before, err)
	}
	return creds, nil
}

func (r *temporaryCredentialRepo) FindByUserID(ctx context.Context, userID uuid.UUID) ([]*model.TemporaryCredential, error) {
	var creds []*model.TemporaryCredential
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ?", userID).
		Find(&creds).Error; err != nil {
		return nil, fmt.Errorf("find temporary credentials for user %s: %w", userID, err)
	}
	return creds, nil
}

func (r *temporaryCredentialRepo) Delete(ctx context.Context, id uuid.UUID) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Delete(&model.TemporaryCredential{}, "id = ?", id).Error; err != nil {
		return fmt.Errorf("delete temporary credential %s: %w", id, err)
	}
	return nil
}

func (r *temporaryCredentialRepo) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("expires_at < ?", before).
		Delete(&model.TemporaryCredential{})
	if result.Error != nil {
		return 0, fmt.Errorf("delete expired temporary credentials before %v: %w", before, result.Error)
	}
	return result.RowsAffected, nil
}

func (r *temporaryCredentialRepo) DeleteByUserID(ctx context.Context, userID uuid.UUID) (int64, error) {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ?", userID).
		Delete(&model.TemporaryCredential{})
	if result.Error != nil {
		return 0, fmt.Errorf("delete temporary credentials for user %s: %w", userID, result.Error)
	}
	return result.RowsAffected, nil
}

// MarkRevoked sets the revoked flag on a credential (soft delete).
// P1-4 fix: Used when cache invalidation fails to ensure revoked credentials
// are rejected even if the cache still contains them.
func (r *temporaryCredentialRepo) MarkRevoked(ctx context.Context, id uuid.UUID) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.TemporaryCredential{}).
		Where("id = ?", id).
		Update("revoked", true).Error; err != nil {
		return fmt.Errorf("mark temporary credential %s as revoked: %w", id, err)
	}
	return nil
}
