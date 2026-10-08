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
	// Delete deletes a credential record.
	Delete(ctx context.Context, id uuid.UUID) error
	// DeleteExpired deletes credentials that expired before the given time.
	// Returns the number of deleted records.
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
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
