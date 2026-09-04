package rtcqueue

import "time"

// WorkStatus represents the lifecycle state of a Work item.
type WorkStatus string

const (
	StatusPending    WorkStatus = "pending"
	StatusProcessing WorkStatus = "processing"
	StatusCompleted  WorkStatus = "completed"
	StatusCancelled  WorkStatus = "cancelled"
)

// Work is a single unit of enqueued labor, scoped to a Session.
type Work struct {
	ID        string     `json:"id" redis:"id"`
	SessionID string     `json:"session_id" redis:"session_id"`
	Data      string     `json:"data" redis:"data"`
	Priority  int64      `json:"priority" redis:"priority"`
	Status    WorkStatus `json:"status" redis:"status"`
	WorkerID  string     `json:"worker_id,omitempty" redis:"worker_id"`
	CreatedAt time.Time  `json:"created_at" redis:"created_at"`
	ClaimedAt time.Time  `json:"claimed_at,omitempty" redis:"claimed_at"`
	UpdatedAt time.Time  `json:"updated_at" redis:"updated_at"`
}

// ClaimResult is the return payload of Queue.Claim.
type ClaimResult struct {
	SessionID string
	WorkID    string
}

// CancelMessage is the payload published on the per-session cancel channel.
type CancelMessage struct {
	WorkID    string `json:"work_id"`
	Reason    string `json:"reason"`
	Timestamp int64  `json:"timestamp"`
}

// Pub/Sub channel names.
const (
	ChannelSessionNew = "session:new"
	// ChannelSessionCancel formats to "session:cancel:%s" with the session id.
	ChannelSessionCancelPrefix = "session:cancel:"
)

func ChannelSessionCancel(sessionID string) string {
	return ChannelSessionCancelPrefix + sessionID
}

// Default lock parameters.
const (
	DefaultLockTTLSeconds   = 120
	DefaultRenewIntervalSec = 30
)

// Redis key prefixes.
const (
	keyPrefixQueue = "queue:session:"
	keyPrefixWork  = "work:"
	keyPrefixLock  = "session:lock:"
	keyPrefixActive = "session:active:"
)

func keyQueue(sessionID string) string  { return keyPrefixQueue + sessionID }
func keyWork(workID string) string      { return keyPrefixWork + workID }
func keyLock(sessionID string) string   { return keyPrefixLock + sessionID }
func keyActive(sessionID string) string { return keyPrefixActive + sessionID }
