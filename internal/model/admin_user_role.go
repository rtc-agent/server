package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AdminUserRole is the join table between admin users and admin roles.
//
// Composite primary key (UserID, RoleID) prevents duplicate assignments.
// Separate indexes on UserID and RoleID enable efficient queries by either dimension.
type AdminUserRole struct {
	UserID     uuid.UUID `gorm:"type:uuid;primaryKey;index:idx_user_roles_user_id" json:"user_id"`
	RoleID     uuid.UUID `gorm:"type:uuid;primaryKey;index:idx_user_roles_role_id" json:"role_id"`
	AssignedAt time.Time `json:"assigned_at"`
}

// TableName specifies the database table name for AdminUserRole.
func (AdminUserRole) TableName() string {
	return "admin_user_roles"
}

// BeforeCreate sets AssignedAt to now if not already set.
// Accepts tx parameter for consistency with other model hooks (e.g., AdminRole.BeforeCreate).
func (ur *AdminUserRole) BeforeCreate(tx *gorm.DB) error {
	if ur.AssignedAt.IsZero() {
		ur.AssignedAt = time.Now()
	}
	return nil
}
