package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DeviceRepo provides device persistence operations.
type DeviceRepo interface {
	// Upsert creates or updates a device record.
	Upsert(ctx context.Context, device *model.Device) error
	// FindByUserAndDeviceID looks up a device by user ID and device ID.
	FindByUserAndDeviceID(ctx context.Context, userID uuid.UUID, deviceID string) (*model.Device, error)
}

type deviceRepo struct {
	db *gorm.DB
}

// NewDeviceRepo creates a new DeviceRepo.
func NewDeviceRepo(db *gorm.DB) DeviceRepo {
	return &deviceRepo{db: db}
}

func (r *deviceRepo) Upsert(ctx context.Context, device *model.Device) error {
	// Atomic upsert via ON CONFLICT to avoid find-then-create race conditions.
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "device_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"name", "user_agent", "last_active_at", "updated_at"}),
		}).
		Create(device).Error
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	return nil
}

func (r *deviceRepo) FindByUserAndDeviceID(ctx context.Context, userID uuid.UUID, deviceID string) (*model.Device, error) {
	var device model.Device
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("user_id = ? AND device_id = ?", userID, deviceID).First(&device).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("find device %s/%s: %w", userID, deviceID, ErrDeviceNotFound)
		}
		return nil, fmt.Errorf("find device %s/%s: %w", userID, deviceID, err)
	}
	return &device, nil
}
