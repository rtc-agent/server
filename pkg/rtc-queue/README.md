# rtc-queue

A thin, Redis-backed distributed queue with session-scoped priority, single-worker session locks, and Pub/Sub-driven workers.

This package exposes only **primitives**: it has no worker loops, no goroutines, no background state. Callers compose these primitives to build their own scheduling policy.

Every mutating operation is a **single atomic Lua script** — no multi-step transactions.

## Concepts

```text
┌──────────────────────────────────────────────────────────┐
│ Session "session-1"                                       │
│                                                           │
│  queue:session:session-1 (ZSET, score = -priority)        │
│     work-A (prio=1)   work-B (prio=10)  work-C (prio=5)   │
│                                                           │
│  session:lock:session-1 = "worker-1"   (TTL 120s)         │
│                                                           │
│  work:work-A  work:work-B  work:work-C   (HASHes)         │
└──────────────────────────────────────────────────────────┘

channel "session:new"            ← fires on every Publish
channel "session:cancel:<sid>"   ← fires on Cancel
```

- **Work**: one unit of labor, belonging to a Session.
- **Session**: a logical grouping. Only one worker at a time can process a session's queue (enforced by the lock).
- **Lock**: per-session, owned by a worker ID, TTL 120s, renewed every 30s. Auto-expires if the worker crashes.

## Install

```go
import rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"

// via go.work replace or:
// require rtc-queue v0.0.0
```

Requires `github.com/redis/go-redis/v9`.

## Quick start

```go
import (
    "context"
    "github.com/redis/go-redis/v9"
    rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
q := rtcqueue.New(rdb)
```

### The easy way: Worker API

If your scheduling needs are standard, use the `Worker` API — it manages subscribe, claim, lock renewal, cancel handling, and graceful shutdown for you. You only provide a callback for the actual work:

```go
w := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{
    WorkerID:    "worker-1",
    Concurrency: 4,              // process up to 4 sessions in parallel
    OnWork: func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
        // do the actual work
        select {
        case cm := <-cancel:
            // admin cancelled this work
            return nil
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(doWork(work.Data)):
            return nil
        }
    },
})

// run until shutdown
go w.Run(ctx)

// graceful shutdown
w.Stop(shutdownCtx)
```

**Worker behavior:**

- Subscribes to `session:new` notifications.
- On each notification, attempts to `Claim` the session (single work item — no draining, all workers re-compete after completion for fairer load distribution).
- Starts a background lock-renewal ticker.
- Listens for cancel notifications on `session:cancel:<sid>`.
- Calls your `OnWork` callback.
- On success, calls `Complete` (releases the lock).
- On error, leaves the work in `processing` state for manual recovery.
- `Stop` cancels active sessions and waits for in-flight work to finish (with timeout).

### The hard way: primitives

If you need unusual scheduling (multi-session fairness, priority aging, custom retry), use the primitive API directly.

### 1. Publish work

```go
workID, err := q.Publish(ctx, "session-1", "payload-here", /*priority=*/10)
// Higher priority = consumed first within a session.
```

`Publish` atomically: writes the work hash, appends to the session's ZSET, and broadcasts on `session:new`.

### 2. Run a worker (primitive API)

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
        continue // lost the race or queue empty
    }

    process(ctx, q, claim)
}
```

### 3. Process with lock renewal (primitive API)

```go
func process(ctx context.Context, q *rtcqueue.Queue, claim *rtcqueue.ClaimResult) {
    // start renewing the lock every 30s in the background
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
                    // lock gone — someone else took over, or we were cancelled
                    return
                }
            }
        }
    }()
    defer close(renewDone)

    // optionally: subscribe to cancel notifications for this session
    cancelSub := q.SubscribeCancel(ctx, claim.SessionID)
    defer cancelSub.Close()
    go func() {
        for msg := range cancelSub.Channel() {
            var cm rtcqueue.CancelMessage
            json.Unmarshal([]byte(msg.Payload), &cm)
            log.Printf("cancel received: work=%s reason=%s", cm.WorkID, cm.Reason)
            // abort work, then return
        }
    }()

    // do the actual work ...
    doWork()

    // mark done; session lock is released atomically
    q.Complete(ctx, claim.WorkID)
}
```

### 4. Drain remaining work in the same session

After completing one work, call `Claim` again — the lock is still held by you, so no other worker can jump in. Keep popping until `Claim` returns `nil`:

```go
for {
    next, _ := q.Claim(ctx, sessionID, "my-worker-id")
    if next == nil { break }
    handle(next)
    q.Complete(ctx, next.WorkID)
}
// lock is still held — call ReleaseSession explicitly, or
// let it expire naturally / let it be re-claimed on next Publish.
```

In practice you often release the lock after the queue is empty so other workers can take the next incoming work. `Complete` already releases the lock, so a simple complete-and-exit is sufficient for most flows.

## Cancel

Cancel is an **admin operation**. Any caller can cancel any work item, and doing so unconditionally releases the session lock — even if a worker is currently processing the item.

```go
err := q.Cancel(ctx, workID, "user requested stop")
// returns ErrAlreadyTerminal if the work is already completed/cancelled
```

- Pending work: removed from the session queue, marked cancelled.
- Processing work: marked cancelled, lock released, a message is published on `session:cancel:<sid>`. The in-flight worker is expected to notice and abort.

Do not expose `Cancel` to untrusted callers.

## Graceful shutdown

```go
sig := make(chan os.Signal, 1)
signal.Notify(sig, syscall.SIGTERM)

<-sig
log.Println("shutting down")

// 1. stop accepting new work
sub.Unsubscribe(ctx, rtcqueue.ChannelSessionNew)

// 2. wait for current work (with timeout)
done := make(chan struct{})
go func() { current.Wait(); close(done) }()
select {
case <-done:
case <-time.After(30 * time.Second):
}

// 3. release the session lock
q.ReleaseSession(ctx, sessionID)
```

Note: `ReleaseSession` is a footgun — it drops any session's lock regardless of caller identity. Only call it on sessions your worker actually owns.

## Crash recovery

If a worker crashes without calling `Complete` or `ReleaseSession`, its session lock remains until the TTL (120s) expires. After expiry, other workers can claim the session normally.

```go
// any worker can now claim
claim, _ := q.Claim(ctx, sessionID, "new-worker")
```

The in-flight work that the crashed worker was processing is **not automatically requeued** — it stays in `status=processing`. This is a deliberate trade-off: partial work is not wasted by being re-executed from scratch. If you need requeue, build it into your application layer.

## Concurrency contract

- `Claim` is atomic — exactly one worker wins a session. Multiple concurrent callers will see at most one succeed.
- `RenewLock` is atomic — it refuses to renew a lock owned by another worker, even under race conditions. (Implemented as a single Lua script.)
- `Publish` / `Complete` / `Cancel` are each a single Lua script.

## Ownership gotchas

- `Complete(workID)` releases the lock on **the session recorded in the work item**, not on any caller-supplied session. Only call `Complete` on work items you yourself claimed.
- `Cancel(workID, ...)` is an admin primitive — it unconditionally yanks the lock out from under the processing worker.

## API reference

### Worker API (recommended for most users)

| Method | Description |
|---|---|
| `NewWorker(q, cfg) *Worker` | Construct a worker. |
| `Worker.Run(ctx) error` | Start processing. Blocks until ctx cancelled. |
| `Worker.Stop(ctx) error` | Graceful shutdown: cancel sessions, wait for in-flight work. |

**WorkerConfig fields:**

| Field | Description |
|---|---|
| `WorkerID string` | Unique worker identity (required). |
| `Concurrency int` | Max sessions processed in parallel (default 1). |
| `OnWork func(ctx, work, cancel) error` | Your work logic. |
| `OnError func(err)` | Error handler (default: log). |
| `RenewInterval time.Duration` | Lock renewal frequency (default 30s). |

### Primitive API (for advanced use)

| Method | Description |
|---|---|
| `Publish(ctx, sessionID, data, priority) (workID, error)` | Enqueue a work item. |
| `Claim(ctx, sessionID, workerID) (*ClaimResult, error)` | Atomically take the next pending work in a session. Returns `nil, nil` if locked or empty. |
| `LoadWork(ctx, workID) (*Work, error)` | Read a work item. Returns `nil, nil` if absent. |
| `Complete(ctx, workID) error` | Mark completed + release session lock. |
| `Cancel(ctx, workID, reason) error` | Admin cancel; releases lock; returns `ErrAlreadyTerminal` for completed/cancelled items. |
| `RenewLock(ctx, sessionID, workerID) (bool, error)` | Atomic lock refresh; returns false if not the owner. |
| `ReleaseSession(ctx, sessionID) error` | Unconditional lock drop. Admin/shutdown use only. |
| `SubscribeNew(ctx) *redis.PubSub` | Subscribe to `session:new`. |
| `SubscribeCancel(ctx, sessionID) *redis.PubSub` | Subscribe to `session:cancel:<sid>`. |

## Status lifecycle

```text
pending ─────► processing ─────► completed
   │                │
   │                └────────► cancelled (via admin Cancel)
   │
   └────────► cancelled (via admin Cancel)
```

## Example

See [example/main.go](./example/main.go) for a complete four-phase demo against miniRedis:

1. Pub/Sub-driven workers consuming work across sessions (primitive API).
2. Ten goroutines racing to claim the same session — exactly one wins.
3. Worker crash simulation with lock-TTL-based recovery.
4. Worker API: callback-driven lifecycle with concurrency, cancel, and graceful shutdown.

Run with:

```bash
go run ./example
```

## Redis key layout

```text
work:{work_id}                      HASH  — work details
queue:session:{session_id}          ZSET  — pending work, scored by -priority
session:lock:{session_id}           STRING — current owner (worker ID), TTL 120s
```

Pub/Sub channels:

```text
session:new                       payload = session_id
session:cancel:{session_id}       payload = CancelMessage JSON
```

## Design notes

- **Why session locks?** Many RTC use cases need "one worker per session" (e.g. a user session must be handled sequentially). The lock enforces this cheaply.
- **Why Lua scripts?** A distributed queue lives or dies by atomicity. Pipelines are not atomic against concurrent writers; Lua scripts are.
- **Why no worker loop in the package?** Scheduling policy (retry backoff, priority aging, multi-session fairness) is highly application-specific. The package exposes only what's universal.
