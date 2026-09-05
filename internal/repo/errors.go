// Package repo 提供数据访问层（Repository）实现。
//
// 所有 repo 方法必须通过 DBFromContext 获取数据库句柄，以支持事务透传。
// 哨兵错误集中在 errors.go 中定义，调用方可通过 errors.Is 判断错误类型。
package repo

import "errors"

// Sentinel errors - 业务方可使用 errors.Is 判断
var (
	// 通用
	ErrNotFound      = errors.New("record not found")
	ErrAlreadyExists = errors.New("record already exists")

	// Session
	ErrSessionNotFound         = errors.New("session not found")
	ErrSessionClosed           = errors.New("session is closed")
	ErrSessionClosedOrNotFound = errors.New("session is closed or not found")

	// Turn
	ErrTurnNotFound   = errors.New("turn not found")
	ErrTurnNotPending = errors.New("turn is not in pending status")

	// Message
	ErrMessageNotFound = errors.New("message not found")

	// Rtc
	ErrRtcNotFound = errors.New("rtc not found")

	// OAuth2User
	ErrOAuth2UserNotFound = errors.New("oauth2 user not found")

	// Device
	ErrDeviceNotFound = errors.New("device not found")

	// RefreshToken
	ErrRefreshTokenNotFound = errors.New("refresh token not found")

	// Permission
	ErrPermissionDenied = errors.New("permission denied")
)

// IsNotFound 判断是否为 not found 类错误
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrSessionNotFound) ||
		errors.Is(err, ErrTurnNotFound) ||
		errors.Is(err, ErrMessageNotFound) ||
		errors.Is(err, ErrRtcNotFound) ||
		errors.Is(err, ErrOAuth2UserNotFound) ||
		errors.Is(err, ErrDeviceNotFound) ||
		errors.Is(err, ErrRefreshTokenNotFound)
}
