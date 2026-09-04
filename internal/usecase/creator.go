// internal/usecase/creator.go
package usecase

import "github.com/google/uuid"

// CreatorKind 动作发起者类型。
type CreatorKind string

const (
	CreatorKindUser   CreatorKind = "user"
	CreatorKindSystem CreatorKind = "system"
	// 未来扩展：
	// CreatorKindAgent CreatorKind = "agent"
)

// Creator 是所有动作发起者的抽象接口。
// 用 interface 而不是 struct，保证后续加 Agent / Device / 第三方集成时不用改调用方。
type Creator interface {
	Kind() CreatorKind
	// ReferenceID 是稳定可存储的标识，用于 DB 列 / 日志 / 查询。
	// UserCreator  → user UUID 字符串
	// SystemCreator → 字面量 "system"
	ReferenceID() string
}

// UserCreator 真实用户发起的动作。
type UserCreator struct {
	UserID   uuid.UUID
	DeviceID string
}

func (c UserCreator) Kind() CreatorKind   { return CreatorKindUser }
func (c UserCreator) ReferenceID() string { return c.UserID.String() }

// SystemCreator 系统发起的动作（LLM / Worker 自动任务）。
// SystemCreator 没有 user/device 概念。
type SystemCreator struct{}

func (c SystemCreator) Kind() CreatorKind   { return CreatorKindSystem }
func (c SystemCreator) ReferenceID() string { return "system" }

// 编译期校验
var (
	_ Creator = UserCreator{}
	_ Creator = SystemCreator{}
)
