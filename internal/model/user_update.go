package model

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UserUpdate 用户更新记录，用于 Topic 频道的离线恢复
// 每条记录对应一批实体变化事件（session/turn/message/rtc 的 created/updated/deleted）
type UserUpdate struct {
	ID        uuid.UUID       `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID       `gorm:"type:uuid;index:idx_user_offset,unique;index;not null"` // 所属用户
	Offset    uint32          `gorm:"index:idx_user_offset,unique;not null"`                 // 用户维度单调递增 offset
	Items     UpdateItemArray `gorm:"type:jsonb;not null"`                                   // 变化条目列表
	DataList  StringArray     `gorm:"type:jsonb" json:"data_list,omitempty"`                 // 富内容缓存（可选）
	CreatedAt time.Time       `gorm:"autoCreateTime;not null"`
}

// TableName 指定表名
func (UserUpdate) TableName() string {
	return "user_updates"
}

func (u *UserUpdate) BeforeCreate(tx *gorm.DB) error {
	if u.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		u.ID = id
	}

	if len(u.Items) == 0 {
		return fmt.Errorf("items must not be empty")
	}

	return nil
}
