// Package model provides domain models.
package model

const (
	// ConfigChannel is the Redis Pub/Sub channel for config update events.
	// When a config is updated, all server instances are notified to invalidate their caches.
	ConfigChannel = "rtc:config_updated"
)

// ConfigUpdateEvent represents a config update event published via Redis Pub/Sub.
// All server instances subscribe to this channel and invalidate their local caches
// when a config they use is updated, ensuring consistency across the cluster.
type ConfigUpdateEvent struct {
	Key       string  `json:"key"`
	UserID    *string `json:"user_id,omitempty"` // nil = system config
	Version   int     `json:"version"`
	Operation string  `json:"operation"` // "update" | "delete" | "rollback"
	Source    string  `json:"source"`    // instance ID or hostname for debugging
}
