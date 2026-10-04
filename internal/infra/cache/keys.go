// Package cache manages all Redis key prefixes and constructor functions centrally.
//
// Conventions:
//   - All key prefix constants are defined in this file. Business code must not hardcode key strings.
//   - Each prefix has a corresponding constructor function (e.g., OAuth2State) that returns the full key string.
//   - New key types must be registered here for global searchability and conflict detection.
package cache

import "fmt"

// ========== Prefix constants ==========

const (
	// PrefixOAuth2State is the OAuth2 CSRF state prefix.
	// Full key: oauth2:state:{state}
	// Value: provider name; TTL: 10 minutes.
	PrefixOAuth2State = "oauth2:state:"

	// PrefixSession is the user session prefix (reserved).
	// Full key: session:{session_id}
	PrefixSession = "session:"

	// PrefixIDEMarker is the idempotent marker prefix (reserved).
	// Full key: idempotent:{domain}:{idempotency_key}
	PrefixIDEMarker = "idempotent:"

	// PrefixChannelOffset is the channel offset counter prefix.
	// Full key: channel:offset:{channel}
	// Value: current max offset (uint64); no TTL (persistent).
	PrefixChannelOffset = "channel:offset:"

	// PrefixChannelEpoch is the channel epoch prefix (immutable after first write).
	// Full key: channel:epoch:{channel}
	// Value: UUID v7 epoch string; no TTL (persistent).
	PrefixChannelEpoch = "channel:epoch:"

	// PrefixSessionMsgOffset is the per-session message global offset counter prefix.
	// Full key: session:msg_offset:{sessionID}
	// Value: current max global_offset (uint64); no TTL (persistent).
	PrefixSessionMsgOffset = "session:msg_offset:"

	// PrefixTurnMsgOffset is the per-turn message offset counter prefix.
	// Full key: session:turn_offset:{turnID}
	// Value: current max turn_offset (uint64); no TTL (persistent).
	PrefixTurnMsgOffset = "session:turn_offset:"

	// PrefixSessionRtcOffset is the per-session RTC record offset counter prefix.
	// Full key: session:rtc_offset:{sessionID}
	// Value: current max rtc_offset (uint64); no TTL (persistent).
	// Separated from the message global_offset counter to avoid RTC creation consuming message offsets and causing gaps.
	PrefixSessionRtcOffset = "session:rtc_offset:"

	// ========== Worker management prefixes ==========
	//
	// Note: the following 5 Worker prefix constants all have the value "worker:", sharing the same root prefix.
	// Distinguishing different data types (info / sessions / queue / background / bg_last_id)
	// relies on suffixes appended in constructor functions (e.g., ":sessions", ":queue").
	// These constants exist as common root prefixes, primarily for documentation and global search.

	// PrefixWorkerInfo is the Worker info Hash prefix.
	// Full key: worker:{workerID}
	PrefixWorkerInfo = "worker:"

	// PrefixWorkerSessions is the Worker's Session Hash prefix.
	// Full key: worker:{workerID}:sessions
	PrefixWorkerSessions = "worker:"

	// PrefixWorkerQueue is the Worker's Turn queue Stream prefix.
	// Full key: worker:{workerID}:queue
	PrefixWorkerQueue = "worker:"

	// PrefixWorkerBackground is the Worker's Background task Stream prefix.
	// Full key: worker:{workerID}:background
	// Separated from the turn stream: background tasks can run concurrently, have no ordering requirements, and do not need session affinity.
	// Not deleted on Worker deregistration (new worker resumes from persisted lastID; idempotent checks prevent duplicate execution).
	PrefixWorkerBackground = "worker:"

	// PrefixWorkerBackgroundLastID is the Worker's Background Stream consumption position prefix.
	// Full key: worker:{workerID}:bg_last_id
	// Value: last consumed Stream ID (format "timestamp-sequence"); no TTL (persistent).
	// Used to resume consumption from the last position after restart, avoiding re-reading historical tasks.
	PrefixWorkerBackgroundLastID = "worker:"

	// SessionAffinityKey is the Session -> Worker mapping Hash.
	// Full key: session:affinity
	SessionAffinityKey = "session:affinity"

	// WorkersActiveKey is the set of all active Workers.
	// Full key: workers:active
	WorkersActiveKey = "workers:active"

	// PrefixTurnCancel is the Turn cancel signal Pub/Sub channel prefix.
	// Full channel: turn:cancel:{turnID}
	// Value: "cancel"
	PrefixTurnCancel = "turn:cancel:"

	// PrefixSessionCancel is the Session-level stop signal Pub/Sub channel prefix.
	// Full channel: session:cancel:{sessionID}
	// Value: "stop"
	// Use case: cross-node session stop for all turns (shared by StopTurn/CloseSession).
	// NOTE: Must stay in sync with rtc-queue/types.go ChannelSessionCancelPrefix.
	PrefixSessionCancel = "session:cancel:"

	// ========== Checkpoint and Interrupt prefixes ==========

	// PrefixCheckpoint is the agent checkpoint data prefix.
	// Full key: checkpoint:{id}
	// Value: checkpoint data (binary); TTL: configurable (recommended 24 hours).
	PrefixCheckpoint = "checkpoint:"

	// PrefixInterruptAnswer is the interrupt answer storage prefix.
	// Full key: interrupt:answer:{sessionID}:{interruptID}
	// Value: answer string; TTL: 10 minutes.
	PrefixInterruptAnswer = "interrupt:answer:"

	// PrefixInterruptChannel is the interrupt answer Pub/Sub channel prefix.
	// Full channel: interrupt:channel:{sessionID}:{interruptID}
	// Value: answer string.
	PrefixInterruptChannel = "interrupt:channel:"

	// PrefixMessageStream is the streaming message chunks List prefix.
	// Full key: message:stream:{messageID}
	// Value: Redis List, each element is a chunk text fragment; TTL: 5 minutes.
	// Use case: temporarily stores incremental chunks during streaming generation; after the last chunk arrives, all chunks are concatenated, written to DB, then the key is deleted.
	PrefixMessageStream = "message:stream:"

	// PrefixRtcResultChannel is the RTC result notification Pub/Sub channel prefix.
	// Full channel: rtc:result:{rtcID}
	// Value: serialized result string.
	// Use case: after RTC completion, notifies the waiting handleInterrupt goroutine via PUBLISH.
	PrefixRtcResultChannel = "rtc:result:"

	// PrefixRtcResultKey is the RTC result storage prefix (SET+PUBLISH pattern).
	// Full key: rtc:result:answer:{rtcID}
	// Value: serialized result string; TTL: 10 minutes.
	// Use case: paired with PUB/SUB to prevent results arriving before subscription (SET then PUBLISH; subscriber does SUBSCRIBE then GET as fallback).
	PrefixRtcResultKey = "rtc:result:answer:"

	// PrefixRtcOrphanTriggered is the orphan turn trigger deduplication prefix (SETNX pattern).
	// Full key: rtc:orphan:triggered:{rtcID}
	// Value: "1"; TTL: 24 hours.
	// Use case: ensures the same RTC only triggers one orphan turn (deduplication when client reports after crash recovery).
	PrefixRtcOrphanTriggered = "rtc:orphan:triggered:"

	// ========== Batch Resume prefixes ==========

	// PrefixRtcBatchPending is the batch resume pending RTC set prefix (Redis Set).
	// Full key: rtc:batch:pending:{turnID}
	// Members: RTC ID list; TTL: 10 minutes.
	// Use case: tracks all RTCs that need interruption in the same turn; triggers batch resume when the set becomes empty.
	PrefixRtcBatchPending = "rtc:batch:pending:"

	// PrefixRtcBatchResults is the batch resume result storage prefix (Redis Hash).
	// Full key: rtc:batch:results:{turnID}
	// Field: RTC ID, value: JSON-encoded result; TTL: 10 minutes.
	// Use case: stores each RTC's completion result for GenResume to build multi-target ResumeParams.
	PrefixRtcBatchResults = "rtc:batch:results:"

	// PrefixRtcBatchInterruptMap is the batch resume InterruptID mapping prefix (Redis Hash).
	// Full key: rtc:batch:interrupt_map:{turnID}
	// Field: RTC ID, value: InterruptID (tool_call_id); TTL: 10 minutes.
	// Use case: stores RTC ID to eino InterruptID mapping for GenResume to build Targets.
	PrefixRtcBatchInterruptMap = "rtc:batch:interrupt_map:"

	// ========== Token estimation prefixes ==========
	// Note: Token estimation data has been migrated to the Session table for persistence; Redis cache is no longer used.

	// ========== Error message rate limiting prefixes ==========

	// PrefixErrorMessageRateLimit is the error message rate limiting counter prefix.
	// Full key: error_msg_rate:{sessionID}:{hour}
	// Value: error message count within the current hour (uint64); TTL: 1 hour.
	// Use case: prevents error message storms caused by Worker failures (max 20 messages per Session per hour).
	PrefixErrorMessageRateLimit = "error_msg_rate:"

	// ========== rtc-oss3 object storage prefixes ==========

	// PrefixOSS3Quota is per-user storage quota counter.
	// Full key: oss3:quota:{user_id}
	// Value: total bytes used (int64); no TTL (persistent, reconciled by cleanup).
	PrefixOSS3Quota = "oss3:quota:"

	// PrefixOSS3QuotaPending is per-user pending (reserved but not yet committed) quota.
	// Used by Lua script OSS3QuotaReserve for two-phase quota allocation.
	// Full key: oss3:quota:pending:{user_id}:{request_id}
	// Value: reserved bytes (int64); TTL: pendingTTL (e.g. 5m, auto-expire to prevent leaks).
	PrefixOSS3QuotaPending = "oss3:quota:pending:"

	// PrefixOSS3QuotaPendingAgg is per-user ZSET that aggregates ALL pending reservations.
	// Solves the multi-concurrent-reserve problem: when multiple Reserve calls happen in
	// parallel for the same user, each call can see the SUM of all other users' pending
	// amounts (not just its own) and correctly enforce the quota limit.
	// Full key: oss3:quota:pending_agg:{user_id}
	// Value: ZSET where member=requestID, score=pending amount;
	//        entries are added by Reserve, removed by Commit/Rollback.
	//        Stale entries (whose pending key expired via TTL) are cleaned up lazily
	//        during Reserve/Commit/Rollback when the pending key GET returns nil.
	// TTL: 0 (self-managing; entries are explicitly removed on Commit/Rollback).
	PrefixOSS3QuotaPendingAgg = "oss3:quota:pending_agg:"

	// PrefixOSS3RateLimit is per-user S3 API rate limiter (ZSET-based sliding window).
	// Full key: oss3:rate:{user_id}
	// Value: ZSET where member=requestID, score=timestamp;
	//        Lua script OSS3RateLimitCheck atomically prunes expired entries and counts.
	// TTL: 0 (managed by ZREMRANGEBYSCORE in Lua; key auto-empty when window slides past).
	PrefixOSS3RateLimit = "oss3:rate:"

	// PrefixOSS3ConcurrentUpload is per-user concurrent multipart upload counter.
	// Full key: oss3:upload:{user_id}
	// Value: active upload count (int64); no TTL.
	PrefixOSS3ConcurrentUpload = "oss3:upload:"

	// PrefixOSS3CredentialCache caches temporary credential lookup by AccessKeyID.
	// Full key: oss3:cred_cache:{access_key_id}
	// Value: JSON {user_id, secret_access_key, expires_at}; TTL: min(remaining TTL, 5m).
	PrefixOSS3CredentialCache = "oss3:cred_cache:"

	// PrefixOSS3Lock is generic distributed lock prefix for OSS3 operations.
	// Used by AcquireLock/ExtendLock/ReleaseLock Lua scripts.
	// Full key: oss3:lock:{resource_name}  (e.g. oss3:lock:cleanup)
	// Value: holderUUID (string); TTL: seconds (configurable per lock).
	PrefixOSS3Lock = "oss3:lock:"

	// PrefixOSS3QuotaCommitMarker is an idempotency marker for quota commits.
	// Full key: oss3:quota_commit:{user_id}:{request_id}
	// Value: "1"; TTL: 24h (covers retry window).
	// SETNX before commit prevents duplicate quota increments on network retries.
	PrefixOSS3QuotaCommitMarker = "oss3:quota_commit:"

	// PrefixJWKS is the JWKS public key cache prefix.
	// Full key: jwks:{issuer}:{kid}
	// Value: JSON-encoded JWK; TTL: configurable (default 1 hour).
	PrefixJWKS = "jwks:"

	// PrefixJWKSLock is the distributed lock prefix for JWKS cache refresh.
	// Full key: jwks:lock:{issuer}
	// Value: holder UUID; TTL: 10 seconds (covers fetch duration).
	// Prevents multiple instances from simultaneously refreshing the same issuer's JWKS.
	PrefixJWKSLock = "jwks:lock:"
)

// ========== Constructor functions ==========

// OAuth2State returns the Redis key for an OAuth2 state.
func OAuth2State(state string) string {
	return PrefixOAuth2State + state
}

// Session returns the Redis key for a session.
func Session(sessionID string) string {
	return PrefixSession + sessionID
}

// IDEMarker returns the Redis key for an idempotent marker.
func IDEMarker(domain, idempotencyKey string) string {
	return fmt.Sprintf("%s%s:%s", PrefixIDEMarker, domain, idempotencyKey)
}

// ChannelOffset returns the Redis key for a channel offset counter.
func ChannelOffset(channel string) string {
	return PrefixChannelOffset + channel
}

// ChannelEpoch returns the Redis key for a channel epoch.
func ChannelEpoch(channel string) string {
	return PrefixChannelEpoch + channel
}

// SessionMsgOffset returns the Redis key for the per-session message global offset counter.
func SessionMsgOffset(sessionID string) string {
	return PrefixSessionMsgOffset + sessionID
}

// TurnMsgOffset returns the Redis key for the per-turn message offset counter.
func TurnMsgOffset(turnID string) string {
	return PrefixTurnMsgOffset + turnID
}

// SessionRtcOffset returns the Redis key for the per-session RTC record offset counter.
func SessionRtcOffset(sessionID string) string {
	return PrefixSessionRtcOffset + sessionID
}

// ========== Worker management constructors ==========

// WorkerInfo returns the Redis key for the Worker info Hash.
func WorkerInfo(workerID string) string { return PrefixWorkerInfo + workerID }

// WorkerSessions returns the Redis key for the Worker's Session Hash.
func WorkerSessions(workerID string) string { return PrefixWorkerSessions + workerID + ":sessions" }

// WorkerQueue returns the Redis key for the Worker's Turn queue Stream.
func WorkerQueue(workerID string) string { return PrefixWorkerQueue + workerID + ":queue" }

// WorkerBackground returns the Redis key for the Worker's Background task Stream.
func WorkerBackground(workerID string) string {
	return PrefixWorkerBackground + workerID + ":background"
}

// WorkerBackgroundLastID returns the Redis key for the Worker's Background Stream consumption position.
func WorkerBackgroundLastID(workerID string) string {
	return PrefixWorkerBackgroundLastID + workerID + ":bg_last_id"
}

// SessionAffinity returns the Redis key for the Session -> Worker mapping Hash.
func SessionAffinity() string { return SessionAffinityKey }

// WorkersActive returns the Redis key for the set of all active Workers.
func WorkersActive() string { return WorkersActiveKey }

// TurnCancel returns the Pub/Sub channel name for a Turn cancel signal.
func TurnCancel(turnID string) string { return PrefixTurnCancel + turnID }

// SessionCancel returns the Pub/Sub channel name for a Session-level stop signal.
func SessionCancel(sessionID string) string { return PrefixSessionCancel + sessionID }

// ========== Checkpoint and Interrupt constructors ==========

// Checkpoint returns the Redis key for agent checkpoint data.
func Checkpoint(id string) string {
	return PrefixCheckpoint + id
}

// InterruptAnswer returns the Redis key for interrupt answer storage.
func InterruptAnswer(sessionID, interruptID string) string {
	return PrefixInterruptAnswer + sessionID + ":" + interruptID
}

// InterruptChannel returns the Pub/Sub channel name for interrupt answers.
func InterruptChannel(sessionID, interruptID string) string {
	return PrefixInterruptChannel + sessionID + ":" + interruptID
}

// MessageStream returns the Redis key for the streaming message chunks List.
func MessageStream(messageID string) string {
	return PrefixMessageStream + messageID
}

// RtcResultChannel returns the Pub/Sub channel name for RTC result notifications.
func RtcResultChannel(rtcID string) string {
	return PrefixRtcResultChannel + rtcID
}

// RtcResultKey returns the Redis key for RTC result storage.
func RtcResultKey(rtcID string) string {
	return PrefixRtcResultKey + rtcID
}

// RtcOrphanTriggered returns the Redis key for orphan turn trigger deduplication.
func RtcOrphanTriggered(rtcID string) string {
	return PrefixRtcOrphanTriggered + rtcID
}

// ========== Batch Resume constructors ==========

// RtcBatchPending returns the Redis key for the batch resume pending RTC set.
func RtcBatchPending(turnID string) string {
	return PrefixRtcBatchPending + turnID
}

// RtcBatchResults returns the Redis key for batch resume result storage.
func RtcBatchResults(turnID string) string {
	return PrefixRtcBatchResults + turnID
}

// RtcBatchInterruptMap returns the Redis key for the batch resume InterruptID mapping.
func RtcBatchInterruptMap(turnID string) string {
	return PrefixRtcBatchInterruptMap + turnID
}

// ========== Token estimation ==========
// Note: Token estimation data has been migrated to the Session table for persistence; Redis cache is no longer used.
// The original TokenEstimate() function has been deleted.

// ========== Error message rate limiting ==========

// ErrorMessageRateLimit returns the Redis key for the error message rate limiting counter.
func ErrorMessageRateLimit(sessionID, hour string) string {
	return PrefixErrorMessageRateLimit + sessionID + ":" + hour
}

// ========== rtc-oss3 object storage ==========

// OSS3Quota returns the Redis key for per-user storage quota counter.
func OSS3Quota(userID string) string { return PrefixOSS3Quota + userID }

// OSS3QuotaPending returns the Redis key for per-user pending quota reservation.
func OSS3QuotaPending(userID, requestID string) string {
	return PrefixOSS3QuotaPending + userID + ":" + requestID
}

// OSS3QuotaPendingAgg returns the Redis key for the per-user pending aggregation ZSET.
// The ZSET tracks ALL pending reservations for a user so that concurrent Reserve calls
// can correctly compute total pending usage (member=requestID, score=amount).
func OSS3QuotaPendingAgg(userID string) string {
	return PrefixOSS3QuotaPendingAgg + userID
}

// OSS3RateLimit returns the Redis key for per-user S3 API rate limiter.
func OSS3RateLimit(userID string) string { return PrefixOSS3RateLimit + userID }

// OSS3ConcurrentUpload returns the Redis key for per-user concurrent upload counter.
func OSS3ConcurrentUpload(userID string) string { return PrefixOSS3ConcurrentUpload + userID }

// OSS3CredentialCache returns the Redis key for temporary credential cache.
func OSS3CredentialCache(accessKeyID string) string { return PrefixOSS3CredentialCache + accessKeyID }

// OSS3Lock returns the Redis key for generic distributed lock.
func OSS3Lock(resource string) string { return PrefixOSS3Lock + resource }

// OSS3QuotaCommitMarker returns the Redis key for quota commit idempotency marker.
func OSS3QuotaCommitMarker(userID, requestID string) string {
	return PrefixOSS3QuotaCommitMarker + userID + ":" + requestID
}

// ========== JWKS cache ==========

// JWKSKey returns the Redis key for a cached JWKS public key.
func JWKSKey(issuer, kid string) string {
	return PrefixJWKS + issuer + ":" + kid
}

// JWKSLockKey returns the Redis key for the JWKS refresh distributed lock.
func JWKSLockKey(issuer string) string {
	return PrefixJWKSLock + issuer
}
