package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/dbmodel"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DeviceRepo 设备仓储接口
type DeviceRepo interface {
	Upsert(ctx context.Context, device *dbmodel.Device) error
	FindByUserAndDeviceID(ctx context.Context, userID uuid.UUID, deviceID string) (*dbmodel.Device, error)
}

type deviceRepo struct {
	db *gorm.DB
}

// NewDeviceRepo 创建 DeviceRepo
func NewDeviceRepo(db *gorm.DB) DeviceRepo {
	return &deviceRepo{db: db}
}

func (r *deviceRepo) Upsert(ctx context.Context, device *dbmodel.Device) error {
	// 原子 upsert：ON CONFLICT (user_id, device_id) DO UPDATE
	// 避免 find-then-create 的竞态条件
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

func (r *deviceRepo) FindByUserAndDeviceID(ctx context.Context, userID uuid.UUID, deviceID string) (*dbmodel.Device, error) {
	var device dbmodel.Device
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("user_id = ? AND device_id = ?", userID, deviceID).First(&device).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("find device %s/%s: %w", userID, deviceID, ErrDeviceNotFound)
		}
		return nil, fmt.Errorf("find device %s/%s: %w", userID, deviceID, err)
	}
	return &device, nil
}
