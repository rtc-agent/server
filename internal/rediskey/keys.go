// Package rediskey 统一管理所有 Redis key 的前缀与构造函数。
//
// 规范：
//   - 所有 key 前缀常量集中在此文件定义，禁止在业务代码中硬编码 key 字符串。
//   - 每个前缀提供对应的构造函数（如 OAuth2State），返回完整的 key 字符串。
//   - 新增 key 类型时必须在此处注册，便于全局检索与冲突检测。
package rediskey

import "fmt"

// ========== 前缀常量 ==========

const (
	// PrefixOAuth2State OAuth2 CSRF state 前缀
	// 完整 key: oauth2:state:{state}
	// value: provider 名称；TTL: 10 分钟
	PrefixOAuth2State = "oauth2:state:"

	// PrefixSession 用户会话前缀（预留）
	// 完整 key: session:{session_id}
	PrefixSession = "session:"

	// PrefixIDEMarker 幂等标记前缀（预留）
	// 完整 key: idempotent:{domain}:{idempotency_key}
	PrefixIDEMarker = "idempotent:"

	// PrefixChannelOffset 频道 offset 计数器前缀
	// 完整 key: channel:offset:{channel}
	// value: 当前最大 offset（uint64）；无 TTL（持久化）
	PrefixChannelOffset = "channel:offset:"

	// PrefixSessionMsgOffset session 内消息全局 offset 计数器前缀
	// 完整 key: session:msg_offset:{sessionID}
	// value: 当前最大 global_offset（uint64）；无 TTL（持久化）
	PrefixSessionMsgOffset = "session:msg_offset:"

	// PrefixTurnMsgOffset turn 内消息 offset 计数器前缀
	// 完整 key: session:turn_offset:{turnID}
	// value: 当前最大 turn_offset（uint64）；无 TTL（持久化）
	PrefixTurnMsgOffset = "session:turn_offset:"

	// ========== Worker 管理相关前缀 ==========

	// PrefixWorkerInfo Worker 信息 Hash 前缀
	// 完整 key: worker:{workerID}
	PrefixWorkerInfo = "worker:"

	// PrefixWorkerSessions Worker 负责的 Session Hash 前缀
	// 完整 key: worker:{workerID}:sessions
	PrefixWorkerSessions = "worker:"

	// PrefixWorkerQueue Worker 的 Turn 队列 Stream 前缀
	// 完整 key: worker:{workerID}:queue
	PrefixWorkerQueue = "worker:"

	// PrefixWorkerBackground Worker 的 Background 任务 Stream 前缀
	// 完整 key: worker:{workerID}:background
	// 与 turn stream 分离：background 任务可并发、无顺序要求，不需要 session 亲和。
	// Worker 注销时不删除（新 worker 启动后从持久化的 lastID 继续，幂等检查保证不重复执行）。
	PrefixWorkerBackground = "worker:"

	// PrefixWorkerBackgroundLastID Worker 的 Background Stream 消费位置前缀
	// 完整 key: worker:{workerID}:bg_last_id
	// value: 最后消费的 Stream ID（格式 "timestamp-sequence"）；无 TTL（持久化）
	// 用于重启后从上次位置继续消费，避免重读历史任务。
	PrefixWorkerBackgroundLastID = "worker:"

	// SessionAffinityKey Session -> Worker 映射 Hash
	// 完整 key: session:affinity
	SessionAffinityKey = "session:affinity"

	// WorkersActiveKey 所有活跃 Worker Set
	// 完整 key: workers:active
	WorkersActiveKey = "workers:active"

	// PrefixTurnCancel Turn 取消信号 Pub/Sub channel 前缀
	// 完整 channel: turn:cancel:{turnID}
	// value: "cancel"
	PrefixTurnCancel = "turn:cancel:"

	// PrefixSessionCancel Session 级停止信号 Pub/Sub channel 前缀
	// 完整 channel: session:cancel:{sessionID}
	// value: "stop"
	// 用途：跨节点停止 session 的所有 turn（StopTurn/CloseSession 复用）
	PrefixSessionCancel = "session:cancel:"

	// ========== Checkpoint 与 Interrupt 相关前缀 ==========

	// PrefixCheckpoint Agent checkpoint 数据前缀
	// 完整 key: checkpoint:{id}
	// value: checkpoint data (binary); TTL: 可配置（推荐 24 小时）
	PrefixCheckpoint = "checkpoint:"

	// PrefixInterruptAnswer Interrupt 答案存储前缀
	// 完整 key: interrupt:answer:{sessionID}:{interruptID}
	// value: answer string; TTL: 10 分钟
	PrefixInterruptAnswer = "interrupt:answer:"

	// PrefixInterruptChannel Interrupt 答案 Pub/Sub channel 前缀
	// 完整 channel: interrupt:channel:{sessionID}:{interruptID}
	// value: answer string
	PrefixInterruptChannel = "interrupt:channel:"

	// PrefixMessageStream 流式消息 chunks List 前缀
	// 完整 key: message:stream:{messageID}
	// value: Redis List，每个元素是一个 chunk 文本片段；TTL: 5 分钟
	// 用途：流式生成期间临时存储增量 chunks，最后一个 chunk 到达后拼接写入 DB 并删除
	PrefixMessageStream = "message:stream:"

	// PrefixRtcResultChannel RTC 结果通知 Pub/Sub channel 前缀
	// 完整 channel: rtc:result:{rtcID}
	// value: serialized result string
	// 用途：RTC 执行完成后，通过 PUBLISH 通知等待中的 handleInterrupt goroutine
	PrefixRtcResultChannel = "rtc:result:"

	// PrefixRtcResultKey RTC 结果存储前缀（SET+PUBLISH 模式）
	// 完整 key: rtc:result:answer:{rtcID}
	// value: serialized result string; TTL: 10 分钟
	// 用途：与 PUB/SUB 配合，防止订阅前已有结果到达（先 SET 再 PUBLISH，订阅者先 SUBSCRIBE 再 GET 兜底）
	PrefixRtcResultKey = "rtc:result:answer:"

	// PrefixRtcOrphanTriggered 孤儿 turn 触发去重前缀（SETNX 模式）
	// 完整 key: rtc:orphan:triggered:{rtcID}
	// value: "1"; TTL: 24 小时
	// 用途：确保同一 RTC 只触发一次孤儿 turn（crash 恢复后客户端上报时去重）
	PrefixRtcOrphanTriggered = "rtc:orphan:triggered:"
)

// ========== 构造函数 ==========

// OAuth2State 返回 OAuth2 state 的 Redis key
func OAuth2State(state string) string {
	return PrefixOAuth2State + state
}

// Session 返回会话的 Redis key
func Session(sessionID string) string {
	return PrefixSession + sessionID
}

// IDEMarker 返回幂等标记的 Redis key
func IDEMarker(domain, idempotencyKey string) string {
	return fmt.Sprintf("%s%s:%s", PrefixIDEMarker, domain, idempotencyKey)
}

// ChannelOffset 返回频道 offset 计数器的 Redis key
func ChannelOffset(channel string) string {
	return PrefixChannelOffset + channel
}

// SessionMsgOffset 返回 session 内消息全局 offset 计数器的 Redis key
func SessionMsgOffset(sessionID string) string {
	return PrefixSessionMsgOffset + sessionID
}

// TurnMsgOffset 返回 turn 内消息 offset 计数器的 Redis key
func TurnMsgOffset(turnID string) string {
	return PrefixTurnMsgOffset + turnID
}

// ========== Worker 管理构造函数 ==========

// WorkerInfo 返回 Worker 信息 Hash 的 Redis key
func WorkerInfo(workerID string) string { return PrefixWorkerInfo + workerID }

// WorkerSessions 返回 Worker 负责的 Session Hash 的 Redis key
func WorkerSessions(workerID string) string { return PrefixWorkerSessions + workerID + ":sessions" }

// WorkerQueue 返回 Worker 的 Turn 队列 Stream 的 Redis key
func WorkerQueue(workerID string) string { return PrefixWorkerQueue + workerID + ":queue" }

// WorkerBackground 返回 Worker 的 Background 任务 Stream 的 Redis key
func WorkerBackground(workerID string) string {
	return PrefixWorkerBackground + workerID + ":background"
}

// WorkerBackgroundLastID 返回 Worker 的 Background Stream 消费位置的 Redis key
func WorkerBackgroundLastID(workerID string) string {
	return PrefixWorkerBackgroundLastID + workerID + ":bg_last_id"
}

// SessionAffinity 返回 Session -> Worker 映射 Hash 的 Redis key
func SessionAffinity() string { return SessionAffinityKey }

// WorkersActive 返回所有活跃 Worker Set 的 Redis key
func WorkersActive() string { return WorkersActiveKey }

// TurnCancel 返回 Turn 取消信号的 Pub/Sub channel name
func TurnCancel(turnID string) string { return PrefixTurnCancel + turnID }

// SessionCancel 返回 Session 级停止信号的 Pub/Sub channel name
func SessionCancel(sessionID string) string { return PrefixSessionCancel + sessionID }

// ========== Checkpoint 与 Interrupt 构造函数 ==========

// Checkpoint 返回 agent checkpoint 数据的 Redis key
func Checkpoint(id string) string {
	return PrefixCheckpoint + id
}

// InterruptAnswer 返回 interrupt 答案存储的 Redis key
func InterruptAnswer(sessionID, interruptID string) string {
	return PrefixInterruptAnswer + sessionID + ":" + interruptID
}

// InterruptChannel 返回 interrupt 答案 Pub/Sub channel name
func InterruptChannel(sessionID, interruptID string) string {
	return PrefixInterruptChannel + sessionID + ":" + interruptID
}

// MessageStream 返回流式消息 chunks List 的 Redis key
func MessageStream(messageID string) string {
	return PrefixMessageStream + messageID
}

// RtcResultChannel 返回 RTC 结果通知 Pub/Sub channel name
func RtcResultChannel(rtcID string) string {
	return PrefixRtcResultChannel + rtcID
}

// RtcResultKey 返回 RTC 结果存储的 Redis key
func RtcResultKey(rtcID string) string {
	return PrefixRtcResultKey + rtcID
}

// RtcOrphanTriggered 返回孤儿 turn 触发去重的 Redis key
func RtcOrphanTriggered(rtcID string) string {
	return PrefixRtcOrphanTriggered + rtcID
}
