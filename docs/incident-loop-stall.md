# Turn Loop 停止问题分析报告

**日期**: 2026-09-22  
**影响 Session**: `01a0c796-1ecc-72c9-80b9-b214e6ef6d68`  
**影响 Loop**: `01a0c796-8fe7-74c7-a12d-b05e6a8a13b7`

## 问题现象

用户报告：Turn loop 莫名其妙停止执行。

## 问题诊断

### 1. Loop 状态异常

```sql
SELECT status, completed_turns, max_turns, last_run_at, asynq_task_id 
FROM loops WHERE id = '01a0c796-8fe7-74c7-a12d-b05e6a8a13b7';
```

结果：
- `status`: active（应该继续执行）
- `completed_turns`: 4（但 max_turns=5，应该执行第 5 轮）
- `last_run_at`: 2026-09-22 05:37:54（第 4 轮完成时间）
- `asynq_task_id`: 5d014ab2-f422-4d1a-b911-70fa0dc8628e（应该在 05:39:54 触发）

**问题**：Loop 卡在 completed_turns=4，没有继续执行第 5 轮。

### 2. 时间线分析

从日志和数据库重建的事件序列：

| 时间 | 事件 | 来源 |
|------|------|------|
| 05:37:54.566 | 第 4 轮完成，调度第 5 轮（delay=2m） | loopWorkflow.scheduled_next |
| 05:39:54.279 | Loop worker 创建通知消息（Turn 5 of 5） | notificationMessage.created |
| 05:39:54.310 | Worker 收到通知 | worker.received_notification |
| 05:39:54.311 | **Worker 尝试 claim，但队列为空** | worker.claim_empty |
| 05:39:54.315 | Turn 被创建（但 claim_empty 已发生） | turns.created_at |
| 05:39:56.612 | LLM 开始处理，但看到旧通知（Turn 2 of 5） | assistant thinking |
| 05:40:09.852 | LLM 执行错误的任务（第 2 轮而非第 5 轮） | script execution |
| 05:40:14.137 | Turn 完成 | turns.completed_at |
| 05:40:14+ | **OnTurnComplete 未被调用** | 无 loopWorkflow 日志 |

### 3. 根本原因

**问题 1：通知消息积累导致 LLM 混淆**

每次 loop 迭代都会在会话历史中创建一条通知消息（user role）。当 LLM 处理对话时，它看到所有历史通知：

```
05:33:08 - Turn 2 of 5
05:35:29 - Turn 3 of 5  
05:37:49 - Turn 4 of 5
05:39:54 - Turn 5 of 5  ← 最新通知
```

LLM 的 thinking 消息显示它引用了旧通知：
> "现在系统提醒说循环应该执行第2轮了（Turn 2 of 5）"

但实际最新通知是 "Turn 5 of 5"。

**问题 2：竞态条件导致 claim_empty**

Loop worker 的执行流程：
1. 创建通知消息（数据库写入）- 05:39:54.279
2. 发布到 rtcqueue（Redis Lua 脚本原子操作）- 应该立即执行
3. Worker 通过 pubsub 收到通知 - 05:39:54.310
4. Worker 尝试 claim - 05:39:54.311 - **队列为空**

尽管 Redis Lua 脚本保证原子性（HSET + ZADD + PUBLISH），但 pubsub 通知可能在 work item 完全可见前被处理。这可能是：
- Redis 复制延迟（如果使用集群）
- 网络延迟
- 或其他未知的竞态条件

**问题 3：Turn 创建但未触发 OnTurnComplete**

尽管 claim_empty，Turn 仍然在 05:39:54.315 被创建并执行。但这个 Turn 不是通过正常的 rtcqueue claim 流程创建的，所以：
- Turn loop 不知道这个 Turn 的存在
- OnTurnComplete hook 没有被调用
- Loop 的 completed_turns 没有递增
- Loop 卡在 active 状态，永远不会继续

## 解决方案预览

### 方案 1：修复通知消息积累（优先级：高）

**问题**：历史通知消息导致 LLM 混淆。

**解决**：在 `normalizeMessagesForLLM` 中过滤 loop 通知消息，只保留最新的。

```go
// internal/agent/message_normalizer.go

func filterLoopNotifications(messages []*turnagent.Message) []*turnagent.Message {
    var latestLoopNotification *turnagent.Message
    var result []*turnagent.Message
    
    // 从后往前遍历，找到最新的 loop 通知
    for i := len(messages) - 1; i >= 0; i-- {
        msg := messages[i]
        if isLoopNotification(msg) {
            if latestLoopNotification == nil {
                latestLoopNotification = msg
            }
            // 跳过旧的通知
            continue
        }
        result = append([]*turnagent.Message{msg}, result...)
    }
    
    // 把最新的通知插回开头（或保持原位置）
    if latestLoopNotification != nil {
        result = append([]*turnagent.Message{latestLoopNotification}, result...)
    }
    
    return result
}

func isLoopNotification(msg *turnagent.Message) bool {
    if msg.Role != "user" {
        return false
    }
    // 检查是否包含 loop 通知标记
    return strings.Contains(msg.Content, "<system-reminder>") &&
           strings.Contains(msg.Content, "scheduled loop task")
}
```

### 方案 2：修复竞态条件（优先级：中）

**问题**：Worker 收到通知时，work item 还不可见。

**解决选项**：

**选项 A：Worker 重试机制**
```go
// pkg/rtc-queue/worker.go

func (w *Worker) processSession(ctx context.Context, sessionID string) {
    claim, err := w.q.Claim(ctx, sessionID, w.cfg.WorkerID)
    if err != nil {
        // ...
    }
    if claim == nil {
        // 队列为空，但可能是竞态条件
        // 等待一小段时间后重试
        select {
        case <-time.After(100 * time.Millisecond):
            // 重试一次
            claim, err = w.q.Claim(ctx, sessionID, w.cfg.WorkerID)
            if err != nil || claim == nil {
                w.log("worker.claim_empty", map[string]any{
                    "session_id": sessionID,
                    "retried":    true,
                })
                return
            }
        case <-ctx.Done():
            return
        }
    }
    // ...
}
```

**选项 B：延迟发布通知**
```go
// pkg/rtc-queue/queue.go

func (q *Queue) Publish(ctx context.Context, sessionID, data string, priority int64) (string, error) {
    // ... 现有的 Lua 脚本逻辑 ...
    
    // 添加一个小延迟，确保 work item 完全可见
    // 注意：这会增加延迟，不是最佳方案
    time.Sleep(10 * time.Millisecond)
    
    // 或者使用异步通知
    go func() {
        time.Sleep(50 * time.Millisecond)
        q.rdb.Publish(ctx, ChannelSessionNew, sessionID)
    }()
    
    return workID, nil
}
```

**推荐选项 A**，因为它不增加正常路径的延迟。

### 方案 3：确保 OnTurnComplete 被调用（优先级：高）

**问题**：Turn 创建但未触发 OnTurnComplete。

**解决**：在 Turn 完成时，检查是否有相关的 loop 需要更新。

```go
// internal/agent/turn_loop.go 或类似文件

func (l *TurnLoop) completeTurn(ctx context.Context, turnID uuid.UUID) error {
    // 现有的完成逻辑
    
    // 检查这个 Turn 是否属于某个 loop
    // 如果是，调用 loop 的 OnTurnComplete
    turn, err := l.turnRepo.GetByID(ctx, turnID)
    if err != nil {
        return err
    }
    
    // 查找这个 session 的 active loop
    loop, err := l.loopRepo.FindActive(ctx, turn.SessionID)
    if err != nil {
        return err
    }
    
    if loop != nil {
        // 调用 loop 的 OnTurnComplete
        if err := l.loopWorkflow.OnTurnComplete(ctx); err != nil {
            l.logger.Error(ctx, "loop.OnTurnComplete failed", 
                "loop_id", loop.ID, "error", err)
            // 即使失败也要继续，避免阻塞 Turn 完成
        }
    }
    
    return nil
}
```

### 方案 4：Loop 状态恢复机制（优先级：低）

**问题**：Loop 卡在 active 状态，无法自动恢复。

**解决**：添加一个恢复机制，检测并修复卡住的 loop。

```go
// internal/loop/recovery.go

func (r *Recovery) RecoverStaleLoops(ctx context.Context) error {
    // 查找所有 active 的 loop
    loops, err := r.loopRepo.FindAllActive(ctx)
    if err != nil {
        return err
    }
    
    for _, loop := range loops {
        // 检查 loop 是否卡住
        // 条件：last_run_at 超过 interval_seconds 但没有新的 asynq task
        if time.Since(loop.LastRunAt) > time.Duration(loop.IntervalSeconds)*time.Second {
            // 检查是否有 pending 的 asynq task
            taskID := loop.AsynqTaskID
            if taskID == "" || !r.hasPendingTask(ctx, taskID) {
                // Loop 卡住了，重新调度
                r.logger.Warn(ctx, "recovering stale loop",
                    "loop_id", loop.ID,
                    "last_run_at", loop.LastRunAt)
                
                // 重新调度下一个 task
                if err := r.rescheduleLoop(ctx, loop); err != nil {
                    r.logger.Error(ctx, "failed to reschedule loop",
                        "loop_id", loop.ID, "error", err)
                }
            }
        }
    }
    
    return nil
}
```

## 推荐实施顺序

1. **立即修复**：方案 1（过滤旧通知）+ 方案 3（确保 OnTurnComplete 被调用）
2. **短期优化**：方案 2（Worker 重试机制）
3. **长期改进**：方案 4（Loop 恢复机制）

## 临时解决方案

对于当前卡住的 loop，可以手动更新数据库：

```sql
-- 标记 loop 为 exhausted（因为已经执行了 5 轮，尽管第 5 轮执行错误）
UPDATE loops 
SET status = 'exhausted', 
    completed_turns = 5,
    updated_at = NOW()
WHERE id = '01a0c796-8fe7-74c7-a12d-b05e6a8a13b7';
```

或者重新调度：

```sql
-- 清除旧的 asynq_task_id，让恢复机制重新调度
UPDATE loops 
SET asynq_task_id = '',
    last_run_at = NOW() - interval '120 seconds'
WHERE id = '01a0c796-8fe7-74c7-a12d-b05e6a8a13b7';
```

## 后续行动

- [ ] 实施方案 1：过滤旧通知消息
- [ ] 实施方案 3：确保 OnTurnComplete 被调用
- [ ] 实施方案 2：Worker 重试机制
- [ ] 添加单元测试覆盖这些场景
- [ ] 添加监控指标检测卡住的 loop
- [ ] 考虑重构 loop 通知机制（使用单一通知消息，更新而非创建）
