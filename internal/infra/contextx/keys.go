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

// ---------------------------------------------------------------------------
// OSS3 (S3-compatible storage) context keys
// ---------------------------------------------------------------------------
//
// OSS3 uses string-based user IDs (from session tokens) rather than uuid.UUID,
// and needs additional request-scoped keys (request_id, operation). These are
// kept in the same package to maintain a single source of truth for all
// cross-layer context keys, while preserving type safety via distinct key types.

var (
	oss3UserIDKey     = contextKey{"oss3_user_id"}
	oss3RequestIDKey  = contextKey{"oss3_request_id"}
	oss3OperationKey  = contextKey{"oss3_operation"}
	oss3ParsedPathKey = contextKey{"oss3_parsed_path"}
)

// OSS3ParsedPath holds the bucket and key extracted from an S3 path-style URL.
// Stored in request context by OSS3Handler.ServeHTTP to avoid redundant parsing
// in downstream handlers (e.g., OSS3MultipartHandler.ServeHTTP).
type OSS3ParsedPath struct {
	Bucket string
	Key    string
}

// GetOSS3UserID retrieves the OSS3 user ID (string) from the context.
func GetOSS3UserID(ctx context.Context) string {
	id, _ := ctx.Value(oss3UserIDKey).(string)
	return id
}

// WithOSS3UserID injects the OSS3 user ID into the context.
func WithOSS3UserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, oss3UserIDKey, userID)
}

// GetOSS3RequestID retrieves the OSS3 request ID from the context.
func GetOSS3RequestID(ctx context.Context) string {
	id, _ := ctx.Value(oss3RequestIDKey).(string)
	return id
}

// WithOSS3RequestID injects the OSS3 request ID into the context.
func WithOSS3RequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, oss3RequestIDKey, requestID)
}

// GetOSS3Operation retrieves the OSS3 S3 operation name from the context.
func GetOSS3Operation(ctx context.Context) string {
	op, _ := ctx.Value(oss3OperationKey).(string)
	return op
}

// WithOSS3Operation injects the OSS3 S3 operation name into the context.
func WithOSS3Operation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, oss3OperationKey, operation)
}

// GetOSS3ParsedPath retrieves the pre-parsed S3 bucket/key from the context.
// Returns (nil, false) if the path has not been stored yet.
func GetOSS3ParsedPath(ctx context.Context) (*OSS3ParsedPath, bool) {
	pp, ok := ctx.Value(oss3ParsedPathKey).(*OSS3ParsedPath)
	return pp, ok
}

// WithOSS3ParsedPath stores a pre-parsed S3 bucket/key in the context.
func WithOSS3ParsedPath(ctx context.Context, pp *OSS3ParsedPath) context.Context {
	return context.WithValue(ctx, oss3ParsedPathKey, pp)
}
