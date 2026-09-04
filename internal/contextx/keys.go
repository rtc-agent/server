// Package contextx 提供跨层共享的 context key 定义。
// 解耦 middleware / svc / rpchandler 之间的直接依赖。
package contextx

import (
	"context"

	"github.com/google/uuid"
)

// contextKey 使用 unexported struct 类型，防止外部包构造碰撞 key。
type contextKey struct{ name string }

var (
	userIDKey  = contextKey{"user_id"}
	deviceIDKey = contextKey{"device_id"}
)

// GetUserID 从 context 获取用户 ID
func GetUserID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userIDKey).(uuid.UUID)
	return id, ok
}

// GetDeviceID 从 context 获取设备 ID
func GetDeviceID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(deviceIDKey).(string)
	return id, ok
}

// WithClientInfo 向 context 注入用户身份信息
func WithClientInfo(ctx context.Context, userID uuid.UUID, deviceID string) context.Context {
	ctx = context.WithValue(ctx, userIDKey, userID)
	ctx = context.WithValue(ctx, deviceIDKey, deviceID)
	return ctx
}
