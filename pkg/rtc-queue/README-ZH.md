# rtc-queue

基于 Redis 的轻量级分布式队列，支持会话（Session）级别的优先级、单 Worker 会话锁，以及基于 Pub/Sub 的 Worker 驱动。

本包只提供**原子操作原语**：没有 worker 循环、没有 goroutine、没有后台状态。调用方组合这些原语来构建自己的调度策略。

所有写操作都是**单条 Lua 原子脚本**——没有多步事务。

## 核心概念

```text
┌──────────────────────────────────────────────────────────┐
│ Session "session-1"                                       │
│                                                           │
│  queue:session:session-1 (ZSET, score = -priority)        │
│     work-A (prio=1)   work-B (prio=10)  work-C (prio=5)   │
│                                                           │
│  session:lock:session-1 = "worker-1"   (TTL 120s)         │
│                                                           │
│  work:work-A  work:work-B  work:work-C   (HASH)           │
└──────────────────────────────────────────────────────────┘

channel "session:new"            ← 每次 Publish 时触发
channel "session:cancel:<sid>"   ← 每次 Cancel 时触发
```

- **Work**：一个工作单元，属于某个 Session。
- **Session**：逻辑分组。同一时间只有一个 worker 能处理某个 session 的队列（通过锁保证）。
- **Lock**：每个 session 一把锁，归属某个 worker ID，TTL 120 秒，每 30 秒续约一次。Worker 崩溃时自动过期。

## 安装

```go
import rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"

// 通过 go.work replace 或者：
// require rtc-queue v0.0.0
```

依赖 `github.com/redis/go-redis/v9`。

## 快速开始

```go
import (
    "context"
    "github.com/redis/go-redis/v9"
    rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
q := rtcqueue.New(rdb)
```

### 简单方式：Worker API

如果你的调度需求比较标准，使用 `Worker` API——它帮你管理订阅、抢占、锁续约、取消处理和优雅停机。你只需要提供一个回调来处理实际工作：

```go
w := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{
    WorkerID:    "worker-1",
    Concurrency: 4,              // 最多并行处理 4 个 session
    OnWork: func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
        // 实际的工作逻辑
        select {
        case cm := <-cancel:
            // 管理员取消了该任务
            return nil
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(doWork(work.Data)):
            return nil
        }
    },
})

// 运行直到停机
go w.Run(ctx)

// 优雅停机
w.Stop(shutdownCtx)
```

**Worker 行为：**

- 订阅 `session:new` 通知。
- 收到通知时，尝试 `Claim` 该 session（**每次只处理一个 work——不排空队列**，完成后所有 worker 重新竞争，实现更公平的负载分布）。
- 启动后台锁续约 ticker。
- 监听 `session:cancel:<sid>` 上的取消通知。
- 调用你的 `OnWork` 回调。
- 成功时调用 `Complete`（释放锁）。
- 出错时将 work 留在 `processing` 状态，等待人工恢复。
- `Stop` 取消活跃的 session 并等待进行中的 work 完成（带超时）。

### 进阶方式：原语 API

如果你需要特殊的调度策略（多 session 公平性、优先级老化、自定义重试），直接使用原语 API。

### 1. 发布任务（原语 API）

```go
workID, err := q.Publish(ctx, "session-1", "payload-here", /*priority=*/10)
// priority 越大，同一 session 内越先被消费
```

`Publish` 原子地：写入 work hash，追加到 session 的 ZSET，并在 `session:new` 上广播通知。

### 2. 运行 Worker（原语 API）

```go
sub := q.SubscribeNew(ctx)
defer sub.Close()

for msg := range sub.Channel() {
    sessionID := msg.Payload

    claim, err := q.Claim(ctx, sessionID, "my-worker-id")
    if err != nil {
        log.Printf("claim: %v", err)
        continue
    }
    if claim == nil {
        continue // 抢占失败或队列为空
    }

    process(ctx, q, claim)
}
```

### 3. 处理任务并续约锁（原语 API）

```go
func process(ctx context.Context, q *rtcqueue.Queue, claim *rtcqueue.ClaimResult) {
    // 后台每 30 秒续约一次锁
    renewDone := make(chan struct{})
    go func() {
        t := time.NewTicker(30 * time.Second)
        defer t.Stop()
        for {
            select {
            case <-renewDone:
                return
            case <-ctx.Done():
                return
            case <-t.C:
                ok, _ := q.RenewLock(ctx, claim.SessionID, "my-worker-id")
                if !ok {
                    // 锁已丢失——被其他 worker 抢走，或被取消
                    return
                }
            }
        }
    }()
    defer close(renewDone)

    // 可选：订阅该 session 的取消通知
    cancelSub := q.SubscribeCancel(ctx, claim.SessionID)
    defer cancelSub.Close()
    go func() {
        for msg := range cancelSub.Channel() {
            var cm rtcqueue.CancelMessage
            json.Unmarshal([]byte(msg.Payload), &cm)
            log.Printf("收到取消通知：work=%s reason=%s", cm.WorkID, cm.Reason)
            // 中止工作并返回
        }
    }()

    // 实际的工作逻辑 ...
    doWork()

    // 标记完成；session 锁会被原子释放
    q.Complete(ctx, claim.WorkID)
}
```

### 4. 消费同一 Session 的剩余任务

完成一个 work 后，再次调用 `Claim`——锁仍然归你，其他 worker 无法插入。持续 pop 直到 `Claim` 返回 `nil`：

```go
for {
    next, _ := q.Claim(ctx, sessionID, "my-worker-id")
    if next == nil { break }
    handle(next)
    q.Complete(ctx, next.WorkID)
}
// 锁仍然持有——显式调用 ReleaseSession，或让其自然过期，
// 或等待下次 Publish 时被重新抢占。
```

实际使用中，队列空后你通常会释放锁，让其他 worker 处理后续到达的任务。`Complete` 已经会释放锁，所以简单的"完成即退出"对大多数场景已足够。

## 取消任务

Cancel 是**管理操作**。任何调用方都可以取消任何 work，且会无条件释放 session 锁——即使 worker 正在处理该任务。

```go
err := q.Cancel(ctx, workID, "user requested stop")
// 如果 work 已完成或已取消，返回 ErrAlreadyTerminal
```

- 待处理（pending）的 work：从 session 队列中移除，标记为 cancelled。
- 处理中（processing）的 work：标记为 cancelled，释放锁，并在 `session:cancel:<sid>` 上发布消息。正在处理的 worker 应该收到通知并中止。

不要将 `Cancel` 暴露给不可信的调用方。

## 优雅停机

```go
sig := make(chan os.Signal, 1)
signal.Notify(sig, syscall.SIGTERM)

<-sig
log.Println("正在停机")

// 1. 停止接收新任务
sub.Unsubscribe(ctx, rtcqueue.ChannelSessionNew)

// 2. 等待当前任务完成（带超时）
done := make(chan struct{})
go func() { current.Wait(); close(done) }()
select {
case <-done:
case <-time.After(30 * time.Second):
}

// 3. 释放 session 锁
q.ReleaseSession(ctx, sessionID)
```

注意：`ReleaseSession` 是个危险操作——它会无条件删除任何 session 的锁，不校验调用方身份。只在你 worker 实际拥有的 session 上调用。

## 崩溃恢复

如果 worker 崩溃而没有调用 `Complete` 或 `ReleaseSession`，它的 session 锁会保留直到 TTL（120 秒）过期。过期后，其他 worker 可以正常抢占该 session。

```go
// 任何 worker 现在都可以抢占
claim, _ := q.Claim(ctx, sessionID, "new-worker")
```

崩溃时正在处理的 work **不会自动重新入队**——它保持在 `status=processing`。这是有意为之的设计取舍：部分执行的工作不会被从头重新执行。如果需要 requeue，请在应用层实现。

## 并发保证

- `Claim` 是原子的——同一 session 只有一个 worker 能成功。多个并发调用方最多只有一个成功。
- `RenewLock` 是原子的——即使存在竞态，也拒绝续约归属其他 worker 的锁。（通过单条 Lua 脚本实现。）
- `Publish` / `Complete` / `Cancel` 各自都是单条 Lua 脚本。

## 所有权陷阱

- `Complete(workID)` 释放的是 **work 项中记录的 session** 的锁，而不是调用方传入的 session。只对你自己 claim 的 work 调用 `Complete`。
- `Cancel(workID, ...)` 是管理原语——它会无条件地从正在处理的 worker 手中夺走锁。

## API 参考

### Worker API（推荐大多数用户使用）

| 方法 | 说明 |
| --- | --- |
| `NewWorker(q, cfg) *Worker` | 构造 worker。 |
| `Worker.Run(ctx) error` | 开始处理。阻塞直到 ctx 取消。 |
| `Worker.Stop(ctx) error` | 优雅停机：取消 session，等待进行中的 work。 |

**WorkerConfig 字段：**

| 字段 | 说明 |
| --- | --- |
| `WorkerID string` | 唯一的 worker 标识（必填）。 |
| `Concurrency int` | 最大并行处理的 session 数（默认 1）。 |
| `OnWork func(ctx, work, cancel) error` | 你的工作逻辑。 |
| `OnError func(err)` | 错误处理（默认：log）。 |
| `RenewInterval time.Duration` | 锁续约频率（默认 30s）。 |

### 原语 API（用于高级场景）

| 方法 | 说明 |
| --- | --- |
| `Publish(ctx, sessionID, data, priority) (workID, error)` | 入队一个 work 项。 |
| `Claim(ctx, sessionID, workerID) (*ClaimResult, error)` | 原子抢占 session 中下一个待处理 work。锁住或空队列时返回 `nil, nil`。 |
| `LoadWork(ctx, workID) (*Work, error)` | 读取 work 项。不存在时返回 `nil, nil`。 |
| `Complete(ctx, workID) error` | 标记完成 + 释放 session 锁。 |
| `Cancel(ctx, workID, reason) error` | 管理取消；释放锁；对已完成/已取消项返回 `ErrAlreadyTerminal`。 |
| `RenewLock(ctx, sessionID, workerID) (bool, error)` | 原子续约锁；非所有者时返回 false。 |
| `ReleaseSession(ctx, sessionID) error` | 无条件释放锁。仅用于管理/停机。 |
| `SubscribeNew(ctx) *redis.PubSub` | 订阅 `session:new`。 |
| `SubscribeCancel(ctx, sessionID) *redis.PubSub` | 订阅 `session:cancel:<sid>`。 |

## 状态生命周期

```text
pending ─────► processing ─────► completed
   │                │
   │                └────────► cancelled（通过管理 Cancel）
   │
   └────────► cancelled（通过管理 Cancel）
```

## 示例

参见 [example/main.go](./example/main.go)，基于 miniRedis 的完整四阶段演示：

1. Pub/Sub 驱动的 worker 跨多个 session 消费任务（原语 API）。
2. 十个 goroutine 抢占同一 session——恰好一个胜出。
3. Worker 崩溃模拟，基于锁 TTL 的恢复。
4. Worker API：回调驱动的生命周期，支持并发、取消和优雅停机。

运行：

```bash
go run ./example
```

## Redis Key 布局

```text
work:{work_id}                      HASH  — work 详情
queue:session:{session_id}          ZSET  — 待处理 work，按 -priority 排序
session:lock:{session_id}           STRING — 当前所有者（worker ID），TTL 120s
```

Pub/Sub 通道：

```text
session:new                       payload = session_id
session:cancel:{session_id}       payload = CancelMessage JSON
```

## 设计说明

- **为什么需要 session 锁？** 很多 RTC 场景需要"一个 session 对应一个 worker"（例如用户 session 必须顺序处理）。锁用很低的成本实现了这一点。
- **为什么用 Lua 脚本？** 分布式队列的生死在于原子性。Pipeline 对并发写者不是原子的；Lua 脚本是。
- **为什么包内不内置 worker 循环？** 调度策略（重试退避、优先级老化、多 session 公平性）高度依赖具体应用。本包只提供通用的部分。
