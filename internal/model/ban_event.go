// Package model provides domain models.
package model

const (
	// BanChannel is the Redis Pub/Sub channel for user ban events.
	BanChannel = "rtc:user_banned"
)

// BanEvent represents a user ban/unban event published via Redis Pub/Sub.
type BanEvent struct {
	UserID string `json:"user_id"`
	Action string `json:"action"` // "ban" or "unban"
	Reason string `json:"reason,omitempty"`
}
