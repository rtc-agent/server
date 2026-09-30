package repo

import (
	"context"
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
	return r.db.WithContext(ctx).Create(cred).Error
}

func (r *temporaryCredentialRepo) GetByAccessKeyID(ctx context.Context, accessKeyID string) (*model.TemporaryCredential, error) {
	var cred model.TemporaryCredential
	err := r.db.WithContext(ctx).Where("access_key_id = ?", accessKeyID).First(&cred).Error
	if err != nil {
		return nil, err
	}
	return &cred, nil
}

func (r *temporaryCredentialRepo) FindExpired(ctx context.Context, before time.Time) ([]*model.TemporaryCredential, error) {
	var creds []*model.TemporaryCredential
	err := r.db.WithContext(ctx).
		Where("expires_at < ?", before).
		Find(&creds).Error
	return creds, err
}

func (r *temporaryCredentialRepo) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).
		Delete(&model.TemporaryCredential{}, "id = ?", id).Error
}

func (r *temporaryCredentialRepo) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	result := r.db.WithContext(ctx).
		Where("expires_at < ?", before).
		Delete(&model.TemporaryCredential{})
	return result.RowsAffected, result.Error
}
