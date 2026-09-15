# 性能监控使用指南

## 概览

server 项目已集成三组性能监控能力：

| 能力 | 端点 | 描述 |
|------|------|------|
| pprof | `/debug/pprof/` | CPU / 内存 / goroutine 堆栈 profiling |
| goroutine 监控 | `/debug/goroutines` | goroutine 数量与状态分布 |
| HTTP 指标 | `/metrics` | Prometheus 格式的请求延迟、响应大小、状态码分布 |

所有 debug 端点可通过配置 `debug.enabled: false` 完全关闭。

## 配置

在 `etc/config.yaml`（或环境变量）中添加：

```yaml
debug:
  enabled: true                              # 是否启用 /debug/* 端点（默认 true）
  user: "admin"                              # Basic Auth 用户名（生产环境必填）
  password: "${DEBUG_PASSWORD}"              # Basic Auth 密码（支持环境变量引用）
  goroutine_leak_threshold: 1000             # goroutine 数量告警阈值（0 禁用）

metrics:
  user: "prom"                               # /metrics 端点 Basic Auth
  password: "${METRICS_PASSWORD}"
```

**安全提醒**：
- 生产环境**必须**配置 `debug.user` 和 `debug.password`
- 未配置认证时，非开发环境会记录 WARN 日志
- 开发环境（`server.env: development`）允许无认证访问

## 端点使用

### 1. pprof 端点

#### 浏览器查看

```
http://localhost:8080/debug/pprof/
```

索引页列出所有可用 profile，点击即可下载或查看。

#### 常用操作

```bash
# CPU profile（30 秒采样）
curl -u admin:pass "http://localhost:8080/debug/pprof/profile?seconds=30" > cpu.prof
go tool pprof -http=:8081 cpu.prof

# 堆内存 profile
curl -u admin:pass "http://localhost:8080/debug/pprof/heap" > heap.prof
go tool pprof -http=:8081 heap.prof

# 所有 goroutine 堆栈
curl -u admin:pass "http://localhost:8080/debug/pprof/goroutine?debug=1"

# 执行 trace（5 秒）
curl -u admin:pass "http://localhost:8080/debug/pprof/trace?seconds=5" > trace.out
go tool trace trace.out
```

#### 内存泄漏排查

```bash
# 1. 抓取当前堆快照
curl -u admin:pass "http://localhost:8080/debug/pprof/heap" > heap1.prof

# 2. 等待一段时间（几分钟）
sleep 300

# 3. 再抓一次
curl -u admin:pass "http://localhost:8080/debug/pprof/heap" > heap2.prof

# 4. 对比差异
go tool pprof -base heap1.prof heap2.prof
```

### 2. goroutine 监控端点

```bash
# 摘要模式（JSON）
curl http://localhost:8080/debug/goroutines
# 输出示例：
# {
#   "goroutine_count": 42,
#   "by_state": {"running": 3, "syscall": 5, "waiting": 34},
#   "timestamp": "2026-09-15T05:42:00Z"
# }

# 详细模式（完整堆栈文本）
curl "http://localhost:8080/debug/goroutines?detail=1"
```

#### Prometheus 指标

- `go_goroutines` — goroutine 总数（由 Prometheus GoCollector 自动提供）
- `rtc_goroutines_by_state{state="..."}` — 按状态分类的 goroutine 数量（每 10 秒采样）

#### 泄漏告警

当 goroutine 数量超过 `debug.goroutine_leak_threshold`（默认 1000）时，
后台采集器会记录 WARN 级别日志：

```
goroutine leak threshold exceeded  current=1523  threshold=1000
```

### 3. HTTP 请求指标

`/metrics` 端点暴露的 HTTP 相关 Prometheus 指标：

| 指标名 | 类型 | 标签 | 描述 |
|--------|------|------|------|
| `http_requests_total` | Counter | method, handler, code | 请求总数 |
| `http_request_duration_seconds` | Histogram | handler | 请求延迟分布 |
| `http_request_size_bytes` | Histogram | - | 请求体大小分布 |
| `http_response_size_bytes` | Histogram | - | 响应体大小分布 |
| `http_in_flight_requests` | Gauge | - | 当前并发请求数 |

实现使用 `github.com/felixge/httpsnoop` 透明包装 ResponseWriter，
确保 WebSocket 升级（http.Hijacker）和流式响应（http.Flusher）正常工作。

自动跳过 `/healthz`、`/metrics` 和 WebSocket 升级请求。

## 测试验证

### 单元测试

```bash
cd server
go test ./internal/infra/middleware/... -v
```

覆盖：
- GoroutinesHandler 摘要/详细模式
- StartGoroutineCollector 启动/停止
- RegisterPprofRoutes 路由注册
- BasicAuth 认证逻辑
- HTTPMetrics 中间件（跳过 healthz、捕获响应、跳过 WebSocket）

### 手动集成测试

```bash
# 启动服务
go run main.go

# 1. 验证 goroutine 端点
curl -s http://localhost:8080/debug/goroutines | jq .

# 2. 验证 pprof 索引
curl -s http://localhost:8080/debug/pprof/ | head -5

# 3. 验证 Prometheus 指标
curl -s http://localhost:8080/metrics | grep -E "go_goroutines|rtc_goroutines_by_state|http_requests_total|http_request_duration"

# 4. 触发一些请求后检查指标
curl http://localhost:8080/healthz
curl -s http://localhost:8080/metrics | grep http_requests_total
```

## 文件清单

### 新增文件

| 文件 | 描述 |
|------|------|
| `internal/infra/middleware/pprof.go` | pprof 路由注册 + BasicAuth 中间件 |
| `internal/infra/middleware/goroutines.go` | goroutine 监控端点 + 后台采集器 |
| `internal/infra/middleware/monitoring_test.go` | 单元测试（9 个用例） |
| `docs/monitoring-guide.md` | 本文档 |

### 修改文件

| 文件 | 变更 |
|------|------|
| `internal/infra/config/config.go` | 新增 `DebugConfig` 结构体 + 默认值 + env var 展开 |
| `internal/infra/middleware/http_metrics.go` | 重构为使用 `httpsnoop`（替代自定义 ResponseWriter） |
| `internal/server/server.go` | 注册 `/debug/*` 路由、启动/停止 goroutine 采集器 |
| `go.mod` | `httpsnoop` 从 indirect 提升为 direct 依赖 |

## 架构图

```
                    ┌─────────────────────────────────────────────┐
                    │              HTTP Server (mux)               │
                    └──────────────────┬──────────────────────────┘
                                       │
          ┌────────────────────────────┼────────────────────────────┐
          │                            │                            │
   ┌──────▼──────┐            ┌────────▼────────┐          ┌───────▼───────┐
   │   /healthz  │            │   /debug/*      │          │   /metrics    │
   │   /readyz   │            │  (optional auth)│          │  (basic auth) │
   └─────────────┘            └────────┬────────┘          └───────────────┘
                                       │
                   ┌───────────────────┼───────────────────┐
                   │                   │                   │
            ┌──────▼──────┐    ┌───────▼───────┐   ┌──────▼──────┐
            │ /debug/     │    │ /debug/       │   │ Prometheus  │
            │ pprof/      │    │ goroutines    │   │ GoCollector │
            │ (pprof.Index)│    │ (JSON/stack) │   │ (auto)      │
            └─────────────┘    └───────────────┘   └─────────────┘
                                                     │
                                              ┌──────▼──────┐
                                              │ rtc_goroutine│
                                              │ s_by_state   │
                                              │ (10s sample) │
                                              └──────────────┘
```
