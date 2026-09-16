// internal/usecase/creator.go
package usecase

import "github.com/google/uuid"

// CreatorKind identifies the origin of an action.
type CreatorKind string

// CreatorKind constants classify who initiated an action.
const (
	// CreatorKindUser means the action was initiated by a human user.
	CreatorKindUser CreatorKind = "user"
	// CreatorKindSystem means the action was initiated by the system (LLM/worker).
	CreatorKindSystem CreatorKind = "system"
	// 未来扩展：
	// CreatorKindAgent CreatorKind = "agent"
)

// Creator is the abstraction for all action initiators.
// It uses an interface rather than a struct to allow future extensions
// (Agent, Device, third-party integrations) without changing call sites.
type Creator interface {
	Kind() CreatorKind
	// ReferenceID returns a stable, storable identifier for the creator.
	// UserCreator returns the user UUID string; SystemCreator returns "system".
	ReferenceID() string
}

// UserCreator represents actions initiated by a human user.
type UserCreator struct {
	UserID   uuid.UUID
	DeviceID string
}

// Kind returns CreatorKindUser.
func (c UserCreator) Kind() CreatorKind { return CreatorKindUser }

// ReferenceID returns the user's UUID as a string.
func (c UserCreator) ReferenceID() string { return c.UserID.String() }

// SystemCreator represents actions initiated by the system (LLM or worker).
// SystemCreator has no user or device identity.
type SystemCreator struct{}

// Kind returns CreatorKindSystem.
func (c SystemCreator) Kind() CreatorKind { return CreatorKindSystem }

// ReferenceID returns the literal string "system".
func (c SystemCreator) ReferenceID() string { return "system" }

// Compile-time interface verification.
var (
	_ Creator = UserCreator{}
	_ Creator = SystemCreator{}
)
