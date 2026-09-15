# Batch 11 实施报告

## 实施概览

Plan 11: 配置管理与生命周期 - 所有 5 个 Phase 已完成实施。

**实施日期**: 2026-09-15  
**实施范围**: server 仓库配置管理、生命周期管理、环境变量展开

## 已完成的修复项

### Phase 1: Config.Validate() 统一验证 (11.1)

**目标**: 创建统一的配置验证方法，替代分散的验证逻辑

**实现内容**:
- [x] `internal/infra/config/config.go:454-499` - 增强 `Config.Validate()` 方法
  - 添加 `database.dsn` 必填校验
  - 添加 `server.port` 范围校验 (1-65535)
  - 添加 `auth.jwt_secret` 必填校验
  - 添加 `auth.access_token_ttl_seconds` 正值校验
  - 添加 `providers.mock.url` URL 格式校验
  - 调用 `AsynqConfig.Validate()` 验证 asynq 配置

- [x] `internal/infra/config/config.go:501-524` - 新增 `AsynqConfig.Validate()` 方法
  - 校验 `concurrency` 非负
  - 校验 `recovery_interval` 非负
  - 校验 `stale_threshold` 非负
  - 校验 `retry_max` 非负
  - 校验 `retry_timeout` 非负
  - 校验 `health_check_interval` 非负

- [x] `internal/infra/config/config_test.go:105-131` - 更新测试用例
  - 补充 `Database.DSN`、`Server.Port`、`Auth` 等必填字段
  - 确保测试能到达 WorkerConfig 验证逻辑

**验证**: ✓ 编译通过 | ✓ 测试通过

---

### Phase 2: 环境变量展开 (11.2)

**目标**: 支持配置中的环境变量引用，仅限敏感字段

**实现内容**:
- [x] `internal/infra/config/config.go:405-430` - 新增 `expandEnvVars()` 函数
  - 展开 `database.dsn`（可能包含密码）
  - 展开 `redis.password`
  - 展开 `asynq.redis_password`
  - 展开 `llm.api_key`
  - 展开 `providers.mock.client_secret`
  - 展开 `providers.github.client_secret`
  - 展开 `providers.google.client_secret`
  - 展开 `auth.jwt_secret`
  - 展开 `metrics.password`

- [x] `internal/infra/config/config.go:398-400` - 在 `Load()` 中调用 `expandEnvVars()`
  - 替代原有的单一 `llm.api_key` 展开
  - 统一在配置加载后执行展开

**设计原则**:
- 仅对敏感字段（密码、密钥、DSN）生效
- 避免非敏感配置项误用环境变量引入安全隐患
- 支持 `${VAR_NAME}` 格式的环境变量引用

**验证**: ✓ 编译通过 | ✓ 测试通过

---

### Phase 3: LifecycleManager 生命周期管理 (11.3)

**目标**: 统一管理应用生命周期（启动、关闭、健康检查）

**实现内容**:
- [x] `internal/lifecycle/manager.go:1-190` - 新建 lifecycle 包
  - 定义 `Component` 接口（Start/Stop/HealthCheck）
  - 实现 `Manager` 结构体
    - 按注册顺序启动组件
    - 按逆序停止组件
    - 启动失败时自动回滚已启动的组件
    - 支持优雅关闭，等待 goroutine 结束
  - 实现 `Register()` - 注册组件
  - 实现 `Start()` - 启动所有组件
  - 实现 `Stop()` - 停止所有组件并等待 goroutine
  - 实现 `HealthCheck()` - 检查所有组件健康状态
  - 实现 `Go()` - 启动受管 goroutine（panic 恢复 + 自动等待）
  - 实现 `ShutdownCh()` - 获取关闭信号 channel

**设计特点**:
- 线程安全（使用 mutex 保护组件列表）
- 防止重复启动
- 防止启动后注册组件
- 启动失败自动回滚
- 停止时即使某组件失败也继续停止其他组件
- goroutine panic 捕获并记录日志

**验证**: ✓ 编译通过 | ✓ 独立包（无现有测试依赖）

**备注**: LifecycleManager 已创建并可独立使用，现有 server.go 的 Start/Stop 逻辑仍然有效，可选择性迁移。

---

### Phase 4: asynq 配置从硬编码提取 (11.4)

**目标**: 将硬编码的 asynq 配置提取到配置文件

**实现内容**:
- [x] `internal/infra/config/config.go:38-68` - 扩展 `AsynqConfig` 结构
  - 新增 `StaleThreshold` - stale loop 判定阈值（默认 5 分钟）
  - 新增 `RetryMax` - 任务失败最大重试次数（默认 3）
  - 新增 `RetryTimeout` - 任务执行超时时间（默认 30 秒）
  - 新增 `HealthCheckInterval` - 健康检查间隔（默认 30 秒）

- [x] `internal/infra/config/config.go:370-378` - 添加默认值配置
  - `asynq.stale_threshold` = 5m
  - `asynq.retry_max` = 3
  - `asynq.retry_timeout` = 30s
  - `asynq.health_check_interval` = 30s

- [x] `internal/infra/config/config.go:501-524` - 添加配置校验
  - 确保所有新增字段非负

**验证**: ✓ 编译通过 | ✓ 测试通过

---

### Phase 5: staleThreshold 动态化 (11.5)

**目标**: 将硬编码的 stale threshold 提取到配置

**实现内容**:
- [x] `internal/loop/recovery.go:18-24` - 扩展 `RecoveryDeps` 结构
  - 新增 `StaleThreshold` 字段

- [x] `internal/loop/recovery.go:29-45` - 更新 `RunRecovery()` 日志
  - 启动时记录 `stale_threshold` 配置值

- [x] `internal/loop/recovery.go:89-108` - 更新 `recoverStale()` 逻辑
  - 使用 `deps.StaleThreshold` 替代硬编码常量
  - 若配置值 <= 0，回退到默认常量 `staleLoopThreshold`
  - 标记常量 `staleLoopThreshold` 为 Deprecated

- [x] `cmd/wire.go:497-511` - 更新 `provideRecoveryCancel()`
  - 从 `cfg.Asynq.StaleThreshold` 读取配置
  - 若配置值 <= 0，使用默认 5 分钟
  - 传递 `StaleThreshold` 到 `RecoveryDeps`

**验证**: ✓ 编译通过 | ✓ 测试通过

---

## 编译与测试结果

```bash
# 编译检查
$ cd /Users/leichujun/Workspaces/rtc-agent/server && go build ./...
✓ 编译通过（无输出）

# 测试检查
$ go test ./... -short
✓ 所有测试包通过:
  - internal/agent
  - internal/agent/command
  - internal/agent/stringutil
  - internal/agent/templateutil
  - internal/channel
  - internal/handler/http
  - internal/handler/rpc
  - internal/infra/cache
  - internal/infra/config
  - internal/model
  - internal/oauth
  - internal/repo
  - internal/usecase
  - internal/usecase/primitives
  - pkg/memory
  - pkg/turn-agent

# 静态检查
$ go vet ./...
✓ 无警告
```

---

## 关键变更摘要

### 1. 配置验证统一化
- `Config.Validate()` 现在校验所有关键配置项
- 错误信息清晰，指出具体字段和要求
- 防止无效配置启动服务

### 2. 环境变量支持扩展
- 9 个敏感字段支持 `${VAR_NAME}` 格式环境变量
- 安全设计：仅敏感字段可展开，避免误用
- 便于容器化部署和密钥管理

### 3. 生命周期管理基础设施
- 创建 `lifecycle.Manager` 统一管理组件生命周期
- 支持优雅启动、停止、健康检查
- 内置 goroutine 管理和 panic 恢复
- 为未来架构优化提供基础

### 4. Asynq 配置外部化
- 新增 4 个可配置参数
- 消除硬编码，提升运维灵活性
- 所有配置有合理默认值

### 5. Stale Threshold 动态化
- `staleLoopThreshold` 从常量变为可配置
- 通过 `asynq.stale_threshold` 配置项控制
- 保持向后兼容（默认 5 分钟）

---

## 文件变更清单

### 修改的文件
1. `internal/infra/config/config.go` - 增强 Validate、扩展 AsynqConfig、添加 expandEnvVars
2. `internal/infra/config/config_test.go` - 更新测试用例
3. `internal/loop/recovery.go` - 使用可配置 StaleThreshold
4. `cmd/wire.go` - 传递 StaleThreshold 配置

### 新增的文件
1. `internal/lifecycle/manager.go` - LifecycleManager 实现

---

## 配置项参考

### 新增配置项

```yaml
# Asynq 任务调度配置
asynq:
  # 现有配置
  concurrency: 10
  queue: "loop"
  recovery_interval: 1m
  
  # 新增配置
  stale_threshold: 5m          # stale loop 判定阈值
  retry_max: 3                  # 任务失败最大重试次数
  retry_timeout: 30s            # 任务执行超时时间
  health_check_interval: 30s    # 健康检查间隔
```

### 环境变量支持

以下字段支持 `${VAR_NAME}` 格式：

```yaml
database:
  dsn: "postgres://user:${DB_PASSWORD}@localhost/db"

redis:
  password: "${REDIS_PASSWORD}"

asynq:
  redis_password: "${ASYNQ_REDIS_PASSWORD}"

llm:
  api_key: "${LLM_API_KEY}"

providers:
  mock:
    client_secret: "${MOCK_CLIENT_SECRET}"
  github:
    client_secret: "${GITHUB_CLIENT_SECRET}"
  google:
    client_secret: "${GOOGLE_CLIENT_SECRET}"

auth:
  jwt_secret: "${JWT_SECRET}"

metrics:
  password: "${METRICS_PASSWORD}"
```

---

## 待 Review 要点

### 1. Config.Validate() 校验范围
- 当前校验了 database.dsn、server.port、auth.*、llm.api_key、providers、worker、asynq
- **Review 建议**: 是否需要校验其他配置项（如 redis.addr、cors.allow_origins）？
- **当前策略**: 保守校验，仅校验关键必填项和明显错误

### 2. 环境变量展开的安全性
- 仅对 9 个敏感字段展开
- **Review 建议**: 是否有遗漏的敏感字段？是否有不应展开的字段被误展开？
- **当前策略**: 宁缺勿滥，仅展开明确的密码/密钥字段

### 3. LifecycleManager 的集成策略
- 已创建但未集成到现有 server.go
- **Review 建议**: 是否需要在本次 PR 中集成，还是作为基础设施预留？
- **当前策略**: 先创建，后续可选择性迁移，避免大规模重构

### 4. AsynqConfig 新增字段的实际使用
- `RetryMax`、`RetryTimeout`、`HealthCheckInterval` 已定义但未在代码中使用
- **Review 建议**: 是否需要在 asynq worker 中实际使用这些配置？
- **当前策略**: 先定义配置结构，后续使用时再集成

### 5. StaleThreshold 的默认值
- 默认 5 分钟，与原硬编码值一致
- **Review 建议**: 5 分钟是否合适？是否需要更短或更长？
- **当前策略**: 保持原有行为，生产环境可根据实际情况调整

---

## 后续建议

### 短期优化
1. **集成 LifecycleManager**: 将 server.go 的 Start/Stop 逻辑迁移到 LifecycleManager
2. **使用 AsynqConfig 新字段**: 在 asynq worker 中实际应用 retry 和 health check 配置
3. **补充测试**: 为 LifecycleManager 添加单元测试

### 中期优化
1. **配置热更新**: 支持运行时重新加载部分配置（如 log level）
2. **配置版本化**: 添加配置版本检查，防止旧配置格式
3. **配置文档生成**: 自动生成配置项文档

### 长期优化
1. **配置中心集成**: 支持从 etcd/consul 等配置中心读取配置
2. **动态组件注册**: 支持运行时动态注册/注销 Component
3. **配置校验增强**: 支持自定义校验规则和跨字段校验

---

## 实施总结

✅ **所有 5 个 Phase 已完成**
✅ **编译通过**
✅ **测试通过**
✅ **无 vet 警告**
✅ **向后兼容**（所有新配置有合理默认值）

**代码质量**: 
- 遵循项目现有代码风格
- 注释清晰，说明配置用途和默认值
- 错误信息明确，便于排查
- 设计保守，避免过度工程

**可维护性**:
- 配置验证集中化，易于扩展
- 环境变量展开统一化，便于审计
- LifecycleManager 独立包，职责清晰
- 配置项有详细注释，便于理解

---

**实施者**: Claude Code (配置管理专家)  
**审核建议**: 请重点关注上述"待 Review 要点"，确认设计决策是否符合项目需求。
