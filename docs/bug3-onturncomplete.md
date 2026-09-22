# Bug 3：OnTurnComplete 未被调用时序图

## 正常流程：Turn 与 Loop 的协作

```mermaid
sequenceDiagram
    participant Q as rtcqueue Worker
    participant TL as Turn Loop
    participant LW as Loop Workflow
    participant DB as Database

    Note over Q: 从队列 claim work
    Q->>TL: 创建 Turn
    Note over TL: Turn 开始执行
    
    loop 执行多轮对话
        TL->>TL: LLM 调用
        TL->>TL: 工具执行
    end
    
    Note over TL: Turn 完成
    TL->>TL: completeTurn()
    TL->>DB: 更新 Turn status=completed
    TL->>LW: 调用 OnTurnComplete(ctx)
    Note over LW: loop_workflow.go:112<br/>OnTurnComplete()
    
    LW->>DB: 查找 active loop
    DB-->>LW: loop (completed_turns=3)
    
    LW->>LW: newTurns = 3 + 1 = 4
    LW->>DB: 更新 loop<br/>completed_turns=4
    
    alt newTurns <= max_turns
        LW->>LW: scheduleNextLoop(delay=2m)
        Note over LW: 调度下一个 asynq 任务
    else newTurns > max_turns
        LW->>DB: 更新 loop status=exhausted
        Note over LW: Loop 完成
    end
```

## 异常流程：Turn 完成但 OnTurnComplete 未触发

```mermaid
sequenceDiagram
    participant ??? as 未知来源
    participant TL as Turn Loop
    participant LW as Loop Workflow
    participant DB as Database

    Note over ???: Turn 通过非正常路径创建<br/>(无日志记录)
    ???->>DB: 创建 Turn
    Note over DB: 05:39:54.315<br/>turn_id=01a0c7a0-b94b...
    
    Note over TL: Turn 开始执行
    TL->>TL: LLM 调用
    Note over TL: 05:39:56.612<br/>LLM 看到错误的通知
    TL->>TL: 执行错误的任务
    Note over TL: 05:40:14.137<br/>Turn 完成
    
    TL->>TL: completeTurn()
    TL->>DB: 更新 Turn status=completed
    
    Note over TL,LW: ❌ OnTurnComplete 未被调用!
    
    Note over DB: Loop 状态保持不变:<br/>status=active<br/>completed_turns=4<br/>last_run_at=05:37:54<br/>asynq_task_id=旧任务ID
    
    Note over LW: Loop 永久卡住<br/>永远不会调度下一个任务
```

## Turn Loop 生命周期

```mermaid
stateDiagram-v2
    [*] --> Pending: 创建 Loop
    
    Pending --> Active: 第一次调度<br/>(asynq task)
    
    Active --> Active: 轮次执行<br/>completed_turns++<br/>调度下一轮
    
    Active --> Exhausted: completed_turns > max_turns
    Active --> Cancelled: 用户取消
    Active --> Paused: 用户暂停
    
    Paused --> Active: 用户恢复
    Cancelled --> [*]
    Exhausted --> [*]
    
    note right of Active
        每次 Turn 完成后
        OnTurnComplete() 被调用
        递增 completed_turns
        调度下一个 asynq task
    end note
    
    note right of Active
        ❌ Bug: 如果 Turn 完成
        但 OnTurnComplete 未被调用
        Loop 会永远卡在 Active 状态
        completed_turns 不会递增
    end note
```

## 代码调用链分析

### 正常的调用链

```go
// 1. rtcqueue worker 处理 work
// pkg/rtc-queue/worker.go
func (w *Worker) processWork(ctx context.Context, claim *ClaimResult) {
    // ...
    workPayload := turnagent.WorkPayload{...}
    
    // 2. 调用 turn agent 处理
    // internal/agent/turn_agent.go
    agent.Process(ctx, workPayload)
    
    // 3. 创建 Turn Loop
    // internal/agent/turn_loop.go
    turnLoop := NewTurnLoop(...)
    
    // 4. 执行 Turn
    turnLoop.Run(ctx)
    
    // 5. Turn 完成后调用 CompleteTurn
    turnLoop.CompleteTurn(ctx)
    
    // 6. CompleteTurn 内部调用 command registry 的 hooks
    // internal/agent/turn_loop.go
    func (l *TurnLoop) CompleteTurn(ctx context.Context) error {
        // ...
        // 调用所有 command 的 OnTurnComplete hook
        l.registry.OnTurnComplete(ctx)
        // ...
    }
    
    // 7. Command registry 调用 LoopWorkflow.OnTurnComplete
    // internal/agent/loop_workflow.go:112
    func (l *LoopWorkflow) OnTurnComplete(ctx command.Context) error {
        // 更新 loop.completed_turns
        // 调度下一个 asynq task
    }
}
```

### 异常的调用链（推测）

```go
// Turn 通过非正常路径创建
// 可能是某个 resume/retry 逻辑
// 没有经过完整的 Turn Loop 流程

??? -> 直接创建 Turn (数据库写入)
    -> LLM 调用
    -> Turn 完成
    -> completeTurn() 被调用
    -> ❌ 但没有调用 registry.OnTurnComplete()

结果：
- Turn 在数据库中 status=completed
- Loop 的 completed_turns 没有递增
- Loop 的 asynq_task_id 指向已触发的旧任务
- Loop 永久卡住
```

## 可能的根因

### 假设 1：Turn 通过 "resume" 路径创建

```mermaid
sequenceDiagram
    participant LW as Loop Worker
    participant Q as rtcqueue
    participant W as Worker
    participant TL as Turn Loop

    LW->>Q: Publish(work)
    Note over Q: work_item 入队
    Q-->>W: PubSub 通知
    Note over W: 收到通知
    W->>Q: Claim()
    Note over Q,W: ❌ 竞态条件<br/>claim_empty
    Note over W: Worker 放弃
    
    Note over Q: 但 work_item 实际上<br/>在 4ms 后到达
    
    Note over Q: work_item 在队列中<br/>但没有 worker 处理
    
    Note over Q,TL: ❓ 某个超时/重试机制<br/>检测到 work_item<br/>直接创建 Turn？
    
    Q->>TL: 直接创建 Turn<br/>(跳过 normal path)
    Note over TL: Turn 执行但不经过<br/>完整的 Turn Loop 流程
    TL->>TL: 完成
    Note over TL: ❌ OnTurnComplete 未调用
```

### 假设 2：多个 Worker 竞争

```mermaid
sequenceDiagram
    participant Q as Redis Queue
    participant W1 as Worker 1<br/>(server-1)
    participant W2 as Worker 2<br/>(server-2)
    participant TL as Turn Loop

    Note over Q: work_item 入队
    Q-->>W1: PubSub 通知
    Q-->>W2: PubSub 通知
    
    Note over W1: 05:39:54.310<br/>收到通知
    Note over W2: 05:39:54.310<br/>收到通知
    
    W1->>Q: Claim() @ 05:39:54.311
    W2->>Q: Claim() @ 05:39:54.312
    
    Note over Q: W1 的请求先到达<br/>但队列为空？<br/>(work_item 还不可见)
    Q-->>W1: ❌ nil (claim_empty)
    
    Note over Q: work_item 此时变得可见
    Note over Q: W2 的请求到达
    Q-->>W2: ✅ work_item
    
    W2->>TL: 创建 Turn
    Note over TL: Turn 在 server-2 执行
    
    Note over W1: server-1 的日志显示<br/>claim_empty
    Note over W2: server-2 的日志应该显示<br/>claim 成功<br/>(但日志不在 tmp.log 中)
    
    TL->>TL: Turn 完成
    Note over TL: OnTurnComplete 应该被调用<br/>(除非 server-2 的 Turn Loop 实现有问题)
```

## 需要验证的问题

1. **server-2 是否成功 claim 了 work_item？**
   - 检查 server-2 的完整日志
   - 检查 Turn 的 client_id 是否来自 server-2

2. **Turn 是否经过完整的 Turn Loop 流程？**
   - 检查是否有 `createTurn.created` 日志（可能在 server-2）
   - 检查是否有 `normalizeMessagesForLLM.applied` 日志

3. **OnTurnComplete 是否被调用？**
   - 检查是否有 `loopWorkflow.onTurnComplete` 日志
   - 检查 Loop 的 `last_run_at` 是否更新

4. **如果 OnTurnComplete 被调用，为什么 completed_turns 没有递增？**
   - 检查是否有事务回滚
   - 检查是否有并发更新冲突
