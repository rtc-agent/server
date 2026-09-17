// Package contextx provides shared context key definitions across layers.
// Decouples direct dependencies between middleware / svc / rpchandler.
package contextx

import (
	"context"

	"github.com/google/uuid"
)

// contextKey uses an unexported struct type to prevent external packages from creating colliding keys.
type contextKey struct{ name string }

var (
	userIDKey   = contextKey{"user_id"}
	deviceIDKey = contextKey{"device_id"}
)

// GetUserID retrieves the user ID from the context.
func GetUserID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userIDKey).(uuid.UUID)
	return id, ok
}

// GetDeviceID retrieves the device ID from the context.
func GetDeviceID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(deviceIDKey).(string)
	return id, ok
}

// WithClientInfo injects user identity information into the context.
func WithClientInfo(ctx context.Context, userID uuid.UUID, deviceID string) context.Context {
	ctx = context.WithValue(ctx, userIDKey, userID)
	ctx = context.WithValue(ctx, deviceIDKey, deviceID)
	return ctx
}
