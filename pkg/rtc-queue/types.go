package rtcqueue

import "time"

// WorkStatus represents the lifecycle state of a Work item.
type WorkStatus string

// Work status constants define the valid states for a Work item.
const (
	// StatusPending indicates the work is enqueued and awaiting a worker.
	StatusPending WorkStatus = "pending"
	// StatusProcessing indicates the work has been claimed by a worker.
	StatusProcessing WorkStatus = "processing"
	// StatusCompleted indicates the work finished successfully.
	StatusCompleted WorkStatus = "completed"
	// StatusCancelled indicates the work was cancelled before completion.
	StatusCancelled WorkStatus = "cancelled"
)

// Work is a single unit of enqueued labor, scoped to a Session.
type Work struct {
	ID         string     `json:"id" redis:"id"`
	SessionID  string     `json:"session_id" redis:"session_id"`
	Data       string     `json:"data" redis:"data"`
	Priority   int64      `json:"priority" redis:"priority"`
	Status     WorkStatus `json:"status" redis:"status"`
	WorkerID   string     `json:"worker_id,omitempty" redis:"worker_id"`
	CreatedAt  time.Time  `json:"created_at" redis:"created_at"`
	ClaimedAt  time.Time  `json:"claimed_at,omitempty" redis:"claimed_at"`
	UpdatedAt  time.Time  `json:"updated_at" redis:"updated_at"`
	Credential string     `json:"-" redis:"-"` // set by Worker after claim; not persisted
}

// ClaimResult is the return payload of Queue.Claim.
type ClaimResult struct {
	SessionID  string
	WorkID     string
	Credential string // 锁的密码，首次 Claim 时生成，后续 Claim 需要带上
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

// ChannelSessionCancel returns the Pub/Sub channel name for cancel
// notifications scoped to the given session.
func ChannelSessionCancel(sessionID string) string {
	return ChannelSessionCancelPrefix + sessionID
}

// Default lock parameters.
const (
	DefaultLockTTLSeconds   = 120
	DefaultRenewIntervalSec = 30
)

// DefaultMaxConsecutiveRenewFailures is the threshold for consecutive Redis
// errors during lock renewal before the lock is considered lost. Both the
// Worker (pkg/rtc-queue) and SessionTurnManager (pkg/turn-agent) share this
// value to maintain consistent split-brain protection across layers.
//
// Transient errors (network blips, connection pool exhaustion) are tolerated
// up to this count; only when consecutive failures reach the threshold is
// the lock deemed lost. An ok==false response (definitive lock loss) triggers
// immediately regardless of the counter.
const DefaultMaxConsecutiveRenewFailures = 3

// ResumeWorkPriority is the priority used for resume work items. rtc-queue's
// priority queue orders by score = -priority, so higher values are claimed
// first. A resume must always outrank a fresh submit to ensure the eino
// checkpoint is still intact when the worker picks it up.
const ResumeWorkPriority int64 = 100

// Redis key prefixes.
const (
	keyPrefixQueue  = "queue:session:"
	keyPrefixWork   = "work:"
	keyPrefixLock   = "session:lock:"
	keyPrefixActive = "session:active:"
)

func keyQueue(sessionID string) string  { return keyPrefixQueue + sessionID }
func keyWork(workID string) string      { return keyPrefixWork + workID }
func keyLock(sessionID string) string   { return keyPrefixLock + sessionID }
func keyActive(sessionID string) string { return keyPrefixActive + sessionID }

// SessionQueueKey returns the Redis key for the given session's pending
// work queue (a sorted set scored by priority/timestamp). Exported so that
// external packages (e.g. turn-agent) can query pending work counts without
// duplicating the key format.
func SessionQueueKey(sessionID string) string { return keyQueue(sessionID) }

// SessionLockKey returns the Redis key for the given session's distributed lock.
// Exported so that external packages (e.g. server) can check lock liveness
// without duplicating the key format.
func SessionLockKey(sessionID string) string { return keyLock(sessionID) }
