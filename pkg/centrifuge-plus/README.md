# centrifuge-plus

基于 [centrifuge v0.38.0](https://github.com/centrifugal/centrifuge) 的增强库，核心提供 `DualBroker`——一个实现了 centrifuge `Broker` 接口的路由器，根据频道类型将消息分发到不同的底层 broker。

## 核心概念

### 两种频道模式

| 模式 | 持久化 | 实时推送 | 适用场景 |
| ---- | ------ | -------- | -------- |
| **Live** | 否 | 是 | 在线状态、打字机效果等高频低延迟场景，用户离线即丢失 |
| **Topic** | 是 | 是 | 聊天记录、通知等需要可靠投递的场景，离线用户上线后可恢复 |

频道路由通过**注册**指定，不通过频道名前缀。接入者在 centrifuge 的 `OnSubscribe` 回调中调用 `DualBroker.RegisterChannelType` 注册频道类型：

```go
client.OnSubscribe(func(e centrifuge.SubscribeEvent, cb centrifuge.SubscribeCallback) {
    if isTopicChannel(e.Channel) {
        broker.RegisterChannelType(e.Channel, centrifugeplus.Topic)
        cb(centrifuge.SubscribeReply{
            Options: centrifuge.SubscribeOptions{EnableRecovery: true},
        }, nil)
    } else {
        broker.RegisterChannelType(e.Channel, centrifugeplus.Live)
        cb(centrifuge.SubscribeReply{}, nil)
    }
})
```

### 设计原则

`centrifuge-plus` 不管理用户系统、频道订阅关系、业务逻辑。仅提供配置和接口，让接入者自行实现。

## DualBroker

`DualBroker` 实现 centrifuge 的 `Broker` 接口，内部持有两个底层 broker，根据频道类型路由：

- **Live** 频道 → 委托给 centrifuge 内置的 `RedisBroker`
- **Topic** 频道 → 委托给自定义的 `TopicBroker`

### 配置

```go
type DualBrokerConfig struct {
    Live  centrifuge.RedisBrokerConfig
    Topic TopicBrokerConfig
}
```

### 使用方式

```go
node, err := centrifuge.New(centrifuge.Config{})
if err != nil {
    log.Fatalf("创建 node 失败: %v", err)
}

broker, err := NewDualBroker(node, DualBrokerConfig{
    Live: centrifuge.RedisBrokerConfig{
        Prefix: "live",
        Shards: []*centrifuge.RedisShard{redisShard},
    },
    Topic: TopicBrokerConfig{
        Prefix:       "topic",
        RedisAddr:    "localhost:6379",
        HistoryStore: myHistoryStore,
    },
})
if err != nil {
    log.Fatalf("创建 DualBroker 失败: %v", err)
}

node.SetBroker(broker)
```

## Topic 模式

### 先落盘再推送模型

Topic 模式采用"先落盘再推送"架构，确保数据一致性：

```text
BatchIncrby → DB 事务写入 → PublishWithOffset
(预分配offset)   (持久化)       (尽力推送)
```

**关键原则**：数据先落盘，再推送。推送是尽力而为（best-effort），失败不影响数据一致性。

### 接入流程

> **重要**：Topic 消息应通过**服务端 RPC handler**发送，而非客户端 centrifuge publish。
> 客户端通过 RPC 请求发送消息，服务端 handler 执行 BatchIncrby → DB 事务 → PublishWithOffset。
> 不要使用 `OnPublish` 回调实现"先落盘再推送"—— centrifuge 在 `OnPublish` 后还会调用 `Broker.Publish()`，导致重复发布。

```go
// === 服务端 RPC handler（正确处理 Topic 消息）===

// 客户端通过 RPC 方法（如 "send.topic"）发送消息
client.OnRPC(func(e centrifuge.RPCEvent, cb centrifuge.RPCCallback) {
    switch e.Method {
    case "send.topic":
        handleSendTopic(ctx, broker, historyStore, client, e, cb)
    default:
        cb(centrifuge.RPCReply{}, centrifuge.ErrorMethodNotFound)
    }
})

func handleSendTopic(ctx context.Context, broker *centrifugeplus.DualBroker,
    historyStore HistoryStore, client *centrifuge.Client,
    e centrifuge.RPCEvent, cb centrifuge.RPCCallback) {

    var req struct {
        Channel string `json:"channel"`
        Data    string `json:"data"`
    }
    if err := json.Unmarshal(e.Data, &req); err != nil {
        cb(centrifuge.RPCReply{}, err)
        return
    }

    // Step 1: 预分配 offset（原子 HINCRBY）
    positions, err := broker.BatchIncrby(ctx, []centrifugeplus.ChannelIncrbyRequest{
        {Channel: req.Channel},
    })
    if err != nil {
        cb(centrifuge.RPCReply{}, err)
        return
    }
    sp := positions[req.Channel]

    // Step 2: DB 事务（接入方自行实现）
    //   db.Begin() → Create(message) → Create(user_updates) → Commit()
    //   如果事务失败，预分配的 offset 产生 gap，客户端跳过该 offset 继续拉取
    if err := db.Transaction(func(tx *gorm.DB) error {
        if err := tx.Create(&Message{Channel: req.Channel, Data: req.Data, Offset: sp.Offset}).Error; err != nil {
            return err
        }
        if err := tx.Create(&UserUpdate{UserID: client.UserID(), Offset: sp.Offset}).Error; err != nil {
            return err
        }
        return nil
    }); err != nil {
        cb(centrifuge.RPCReply{}, err)
        return
    }

    // Step 3: 事务后推送（best-effort，失败不影响数据一致性）
    broker.PublishWithOffset(ctx, req.Channel, []byte(req.Data), centrifuge.PublishOptions{}, sp)

    cb(centrifuge.RPCReply{Data: []byte(fmt.Sprintf(`{"offset":%d}`, sp.Offset))}, nil)
}
```

### TopicBroker

自定义 broker，提供以下核心方法：

| 方法 | 用途 |
| ---- | ---- |
| `BatchIncrby(ctx, reqs)` | 预分配 offset，DB 事务前调用 |
| `PublishWithOffset(ctx, ch, data, opts, sp)` | 使用预分配 offset 推送，DB 事务后调用 |
| `Publish(ch, data, opts)` | 便捷方法，内部自动 BatchIncrby + PublishWithOffset |
| `History(ch, opts)` | 从 HistoryStore 读取历史，StreamPosition 从 Redis meta 读取 |

`Publish` 是便捷方法，适用于不需要 DB 事务的场景（如简单实时通知）。IM 场景应使用 BatchIncrby + DB 事务 + PublishWithOffset。

### 边缘场景

| 场景 | 处理方式 |
| ---- | -------- |
| BatchIncrby 成功，DB 回滚 | offset 产生 gap，客户端跳过该 offset 继续拉取 |
| DB 成功，PublishWithOffset 失败 | 数据在 DB，客户端通过主动拉取发现 |
| Epoch 变化 | PublishWithOffset 返回 error，接入方可重试 |
| 网络重试 | 幂等缓存（result_key）防止重复消息 |

### DualBroker 扩展方法

`DualBroker` 暴露 Topic 模式的扩展方法，路由到内部的 `TopicBroker`：

```go
func (d *DualBroker) BatchIncrby(ctx context.Context, reqs []ChannelIncrbyRequest) (map[string]centrifuge.StreamPosition, error)
func (d *DualBroker) PublishWithOffset(ctx context.Context, ch string, data []byte, opts centrifuge.PublishOptions, sp centrifuge.StreamPosition) error
```

## HistoryStore

接入者实现此接口，提供历史消息的查询能力。是历史消息的**唯一数据源**。

```go
type HistoryStore interface {
    Query(ctx context.Context, channel string, sinceOffset uint32, latestOffset uint32) ([]*centrifuge.Publication, error)
}
```

## Lua 脚本

| 脚本 | 用途 |
| ---- | ---- |
| `lua/incrby_offset.lua` | 批量预分配 offset，原子执行多个 HINCRBY |
| `lua/publish_with_offset.lua` | 使用预分配 offset 发布到 PUB/SUB，校验 epoch 一致性 |

## 分布式链路追踪

`centrifuge-plus` 内置可选的 OpenTelemetry 分布式追踪支持，零值配置下无开销。

### 追踪配置

```go
type TracingConfig struct {
    Enabled  bool
    Provider trace.TracerProvider  // nil 时使用 noop provider
}
```

关闭时（零值），所有埋点使用 `trace.NewNoopTracerProvider()`，零开销。

### Span 命名

- **Span**: `centrifugeplus.{组件}.{operation}`（如 `centrifugeplus.topicbroker.publish`）
- **Attribute**: `centrifugeplus.{属性}`（如 `centrifugeplus.channel`）

### 跨边界传播

PUB/SUB 消息尾缀携带 W3C traceparent：`__p1:{offset}:{epoch}:{data_len}__{data}[__tp:{traceparent}]`

### 所有 Span 类型

| Span | 触发点 |
| ---- | ------ |
| `centrifugeplus.dualbroker.publish` | Publish 路由 |
| `centrifugeplus.topicbroker.publish` | Publish 便捷方法 |
| `centrifugeplus.topicbroker.batch_incrby` | BatchIncrby |
| `centrifugeplus.topicbroker.batch_incrby.lua` | BatchIncrby Lua 脚本 |
| `centrifugeplus.topicbroker.publish_with_offset` | PublishWithOffset |
| `centrifugeplus.topicbroker.publish_with_offset.lua` | PublishWithOffset Lua 脚本 |
| `centrifugeplus.topicbroker.subscribe` | Subscribe |
| `centrifugeplus.topicbroker.unsubscribe` | Unsubscribe |
| `centrifugeplus.topicbroker.publish_join` | PublishJoin |
| `centrifugeplus.topicbroker.publish_leave` | PublishLeave |
| `centrifugeplus.topicbroker.pubsub` | PUB/SUB 消息接收 |
| `centrifugeplus.topicbroker.history` | History 查询 |

### Jaeger 验证

示例程序默认上报 span 至 Jaeger（`localhost:4317` gRPC）。启动：

```bash
docker compose -f deploy/dev/docker-compose.dependencies.yml up -d jaeger
open http://localhost:16686  # Jaeger UI
```

## 完整示例

参见 [example/main.go](example/main.go)，演示了：

1. **Live 消息**：实时推送，不持久化
2. **Topic 消息**：先落盘再推送（BatchIncrby → Save → PublishWithOffset）
3. **离线恢复**：客户端断线重连后通过主动拉取 DB 恢复消息
4. **流式文本**：逐词发送模拟打字机效果

运行方式：

```bash
# 确保本地 Redis 运行
redis-server

# 确保 Jaeger 运行（用于分布式追踪）
docker compose -f deploy/dev/docker-compose.dependencies.yml up -d jaeger

# 启动示例
cd centrifuge-plus/example && go run .
```

## 项目结构

```text
centrifuge-plus/
├── channel_type.go          # ChannelType 枚举
├── dual_broker.go           # DualBroker 实现
├── dual_broker_config.go    # DualBrokerConfig
├── topic_broker.go          # TopicBroker 实现
├── topic_broker_config.go   # TopicBrokerConfig
├── history_store.go         # HistoryStore 接口
├── logger.go                # Logger 接口
├── tracer.go                # TracingConfig + W3C traceparent 编解码
├── lua/
│   ├── incrby_offset.lua        # 批量预分配 offset
│   └── publish_with_offset.lua  # 使用预分配 offset 发布
└── example/                 # 完整示例程序
```
