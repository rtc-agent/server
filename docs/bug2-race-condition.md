# Bug 2：rtcqueue 竞态条件时序图

## 正常流程（Turn 1-4）

```mermaid
sequenceDiagram
    participant LW as Loop Worker<br/>(asynq)
    participant DB as Database
    participant Redis as Redis Queue
    participant W as rtcqueue Worker
    participant LLM as LLM

    Note over LW: 05:37:54.566<br/>第 4 轮完成
    LW->>LW: scheduleNextLoop(delay=2m)
    Note over LW: asynq 调度任务<br/>将在 05:39:54 触发
    
    Note over LW: 05:39:54.000<br/>asynq 任务触发
    LW->>DB: 创建通知消息<br/>(Turn 5 of 5)
    Note over DB: 05:39:54.279<br/>消息写入成功
    LW->>Redis: Publish(work_item)
    Note over Redis: Lua 脚本原子执行:<br/>1. HSET work:xxx<br/>2. ZADD queue<br/>3. PUBLISH session:new
    Redis-->>W: PubSub 通知
    Note over W: 05:39:54.310<br/>收到通知
    W->>Redis: Claim(session_id)
    Redis-->>W: work_item ✓
    Note over W: 05:39:54.312<br/>claim 成功
    W->>DB: 创建 Turn
    W->>LLM: 加载上下文
    LLM-->>W: 执行 Turn 4
    Note over W: Turn 完成<br/>调用 OnTurnComplete
```

## 异常流程（Turn 5）

```mermaid
sequenceDiagram
    participant LW as Loop Worker<br/>(asynq)
    participant DB as Database
    participant Redis as Redis Queue
    participant W as rtcqueue Worker
    participant ??? as 未知来源
    participant LLM as LLM

    Note over LW: 05:39:54.000<br/>asynq 任务触发
    LW->>DB: 创建通知消息<br/>(Turn 5 of 5)
    Note over DB: 05:39:54.279<br/>消息写入成功
    
    LW->>Redis: Publish(work_item)
    Note over Redis: Lua 脚本执行...
    Redis-->>W: PubSub 通知
    Note over W: 05:39:54.310<br/>收到通知
    
    W->>Redis: Claim(session_id)
    Redis-->>W: ❌ nil (队列为空)
    Note over W: 05:39:54.311<br/>claim_empty!!!
    
    Note over W,Redis: 竞态条件发生<br/>PubSub 通知到达时<br/>work_item 还不可见？
    
    Note over ???: 05:39:54.315<br/>Turn 被创建<br/>(无日志记录!)
    ???->>DB: 创建 Turn
    ???->>LLM: 加载上下文
    Note over LLM: 05:39:56.612<br/>LLM 开始处理
    
    Note over LLM: ❌ LLM 看到错误的通知<br/>"Turn 2 of 5"<br/>而非 "Turn 5 of 5"
    
    LLM-->>???: 执行错误的任务<br/>(第 2 轮而非第 5 轮)
    Note over ???: 05:40:14.137<br/>Turn 完成
    
    Note over ???,LW: ❌ OnTurnComplete 未被调用<br/>completed_turns 保持 4<br/>Loop 永久卡住
```

## 问题分析

### 竞态条件可能的原因

1. **Redis PubSub 可靠性问题**
   - PubSub 是 fire-and-forget 模式
   - 如果订阅者在消息发布时短暂断连，会丢失消息
   - 但这里是收到了通知，只是 claim 时队列为空

2. **Redis 复制延迟（如果使用集群）**
   - 写入发生在 master
   - 读取发生在 replica
   - 复制延迟可能导致读取到旧数据

3. **Lua 脚本执行时序**
   - 虽然 Lua 脚本是原子的，但 PUBLISH 在脚本最后执行
   - 理论上 work_item 应该在 PUBLISH 前就已可见
   - 除非...有其他 worker 在 PUBLISH 后立即 claim 走了？

### 关键疑问

```
05:39:54.311  worker.claim_empty  ← 队列为空
05:39:54.315  Turn 被创建         ← 4ms 后 Turn 出现了
```

**问题**：如果 claim_empty，Turn 是如何被创建的？

**假设**：
- 有一个"影子" worker（可能是 server-2 的 worker）在 claim_empty 后立即 claim 成功了
- 或者 work_item 在 claim_empty 后的 4ms 内到达
- 或者 Turn 是通过其他代码路径创建的（不经过 rtcqueue）

需要进一步调查：
1. 检查 server-2 的日志（可能不在 tmp.log 中）
2. 检查是否有其他创建 Turn 的代码路径
3. 检查 Redis 的配置（是否集群模式）
