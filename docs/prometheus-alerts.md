# Prometheus 告警规则配置

## 概述

本文档定义了 RTC Agent Server 的 Prometheus 告警规则配置。这些规则覆盖了安全性、配置管理、性能、资源使用和业务逻辑等多个维度，确保能够及时发现和响应系统异常。

## 导入方式

将 `prometheus-alerts.yml` 文件添加到 Prometheus 配置的 `rule_files` 部分：

```yaml
rule_files:
  - "/path/to/prometheus-alerts.yml"
```

或者使用相对路径（如果文件在 Prometheus 配置目录下）：

```yaml
rule_files:
  - "prometheus-alerts.yml"
```

修改配置后，重新加载 Prometheus：

```bash
# 发送 SIGHUP 信号
kill -HUP <prometheus_pid>

# 或者通过 API
curl -X POST http://localhost:9090/-/reload
```

## 告警级别说明

| 级别 | 含义 | 响应时间 | 示例 |
|------|------|----------|------|
| **critical** | 严重影响服务可用性，需要立即处理 | < 5 分钟 | DB 熔断器开启、HTTP 错误率 > 5% |
| **warning** | 可能影响服务质量，需要尽快处理 | < 30 分钟 | 高延迟、配置降级、队列积压 |

## 告警规则列表

### 1. 安全性告警 (rtc-agent-security)

#### BanCheckFailOpen
- **含义**: Ban 检查服务不可用，系统正在 fail-open（允许可能被封禁的用户连接）
- **指标**: `rtc_auth_security_events_total{event_type="ban_check_fail_open"}`
- **阈值**: 5 分钟内有任何事件
- **持续时间**: 1 分钟
- **级别**: warning
- **影响**: 被封禁的用户可能绕过限制访问系统
- **处理步骤**:
  1. 检查 Redis 连接状态
  2. 检查 ban 服务日志
  3. 确认 ban 缓存是否正常工作
  4. 如有必要，手动封禁已知违规用户

#### AuthFailureSpike
- **含义**: 认证失败率异常升高
- **指标**: `rtc_auth_failures_total`
- **阈值**: 每秒 > 10 次失败
- **持续时间**: 2 分钟
- **级别**: warning
- **影响**: 可能正在遭受凭证填充攻击或 token 滥用
- **处理步骤**:
  1. 检查失败日志，识别攻击来源
  2. 检查是否有 JWT 密钥泄露
  3. 考虑临时增加速率限制
  4. 如有必要，启用 IP 黑名单

### 2. 配置管理告警 (rtc-agent-config)

#### ConfigFallbackToDefaults
- **含义**: 数据库配置查询失败，系统正在使用 YAML 基线配置
- **指标**: `rtc_config_fallback_total`
- **阈值**: 5 分钟内有任何回退事件
- **持续时间**: 2 分钟
- **级别**: warning
- **影响**: 动态配置更改不会生效，使用默认值
- **处理步骤**:
  1. 检查数据库连接状态
  2. 检查查询的配置键是否存在
  3. 验证数据库权限
  4. 检查应用日志中的详细错误

#### CircuitBreakerOpen
- **含义**: 数据库熔断器已开启，所有数据库操作被阻止
- **指标**: `rtc_circuitbreaker_state{name="db", state="open"}`
- **阈值**: 熔断器状态为 open
- **持续时间**: 1 分钟
- **级别**: critical
- **影响**: 数据库完全不可用，所有配置读取使用 YAML 默认值
- **处理步骤**:
  1. **立即**检查数据库服务器状态
  2. 检查网络连接
  3. 检查数据库连接池配置
  4. 查看是否有慢查询导致连接耗尽
  5. 考虑手动重启数据库连接

#### CircuitBreakerFlapping
- **含义**: 熔断器状态频繁变化（在 closed/open/half_open 之间切换）
- **指标**: `rtc_circuitbreaker_state_changes_total{name="db"}`
- **阈值**: 10 分钟内每秒 > 0.5 次变化
- **持续时间**: 5 分钟
- **级别**: warning
- **影响**: 数据库连接不稳定，服务质量波动
- **处理步骤**:
  1. 检查数据库服务器负载
  2. 检查网络稳定性
  3. 调整熔断器阈值配置（如需要）
  4. 分析慢查询日志

### 3. 性能告警 (rtc-agent-performance)

#### HighHTTPLatency
- **含义**: HTTP 请求延迟过高（P95 > 1 秒）
- **指标**: `http_request_duration_seconds`
- **阈值**: P95 延迟 > 1 秒
- **持续时间**: 5 分钟
- **级别**: warning
- **影响**: 用户体验下降
- **处理步骤**:
  1. 识别高延迟的具体 handler
  2. 检查是否有慢数据库查询
  3. 检查 LLM API 延迟
  4. 分析是否有资源瓶颈（CPU、内存、网络）

#### HighHTTPErrorRate
- **含义**: HTTP 5xx 错误率过高
- **指标**: `http_requests_total{code=~"5.."}`
- **阈值**: 错误率 > 5%
- **持续时间**: 2 分钟
- **级别**: critical
- **影响**: 大量请求失败，服务不可用
- **处理步骤**:
  1. **立即**检查应用日志
  2. 识别错误集中的 handler
  3. 检查依赖服务状态（DB、Redis、LLM API）
  4. 考虑回滚最近的部署

#### LLMAPIHighLatency
- **含义**: LLM API 调用延迟过高
- **指标**: `rtc_llm_http_request_duration_seconds`
- **阈值**: P95 延迟 > 30 秒
- **持续时间**: 5 分钟
- **级别**: warning
- **影响**: 用户等待时间过长
- **处理步骤**:
  1. 检查 LLM 提供商状态页面
  2. 检查是否有大模型切换
  3. 分析请求 payload 大小
  4. 考虑启用请求超时

#### LLMAPIErrorRate
- **含义**: LLM API 错误率过高
- **指标**: `rtc_llm_http_requests_total{status="error"}`
- **阈值**: 错误率 > 10%
- **持续时间**: 2 分钟
- **级别**: critical
- **影响**: 用户无法正常对话
- **处理步骤**:
  1. 检查 LLM 提供商状态
  2. 检查 API 密钥是否有效
  3. 检查速率限制
  4. 考虑切换到备用模型

#### DBQueryHighLatency
- **含义**: 数据库查询延迟过高
- **指标**: `rtc_db_query_duration_seconds`
- **阈值**: P95 延迟 > 0.5 秒
- **持续时间**: 5 分钟
- **级别**: warning
- **影响**: 整体服务延迟增加
- **处理步骤**:
  1. 识别慢查询
  2. 检查索引是否缺失
  3. 检查是否有锁竞争
  4. 分析查询计划

### 4. 资源告警 (rtc-agent-resources)

#### GoroutineLeak
- **含义**: Goroutine 数量异常升高，可能存在泄漏
- **指标**: `go_goroutines`
- **阈值**: > 1000
- **持续时间**: 10 分钟
- **级别**: warning
- **影响**: 内存消耗增加，可能导致 OOM
- **处理步骤**:
  1. 生成 goroutine profile
  2. 分析泄漏的 goroutine 来源
  3. 检查 `context.WithoutCancel` 的使用
  4. 重启服务（临时缓解）

#### QueueWaitTimeHigh
- **含义**: 队列等待时间过长
- **指标**: `rtc_queue_wait_duration_seconds`
- **阈值**: P95 > 10 秒
- **持续时间**: 5 分钟
- **级别**: warning
- **影响**: 任务处理延迟
- **处理步骤**:
  1. 检查消费者数量
  2. 检查处理逻辑是否有瓶颈
  3. 考虑增加消费者实例
  4. 检查 Redis 性能

#### QueuePublishErrors
- **含义**: 队列发布失败
- **指标**: `rtc_queue_publish_total{status="error"}`
- **阈值**: 5 分钟内有任何错误
- **持续时间**: 2 分钟
- **级别**: warning
- **影响**: 任务无法入队
- **处理步骤**:
  1. 检查 Redis 连接
  2. 检查 Redis 内存使用
  3. 验证队列配置

#### QueueClaimErrors
- **含义**: 队列领取失败
- **指标**: `rtc_queue_claim_total{status="error"}`
- **阈值**: 5 分钟内有任何错误
- **持续时间**: 2 分钟
- **级别**: warning
- **影响**: 任务无法被处理
- **处理步骤**:
  1. 检查 Redis 连接
  2. 检查消费者日志
  3. 验证队列配置

#### DBQueryErrors
- **含义**: 数据库查询错误
- **指标**: `rtc_db_queries_total{status="error"}`
- **阈值**: 每秒 > 0.1 个错误
- **持续时间**: 2 分钟
- **级别**: warning
- **影响**: 数据操作失败
- **处理步骤**:
  1. 检查错误日志
  2. 识别失败的查询类型
  3. 检查数据库连接
  4. 验证表结构

### 5. 业务逻辑告警 (rtc-agent-business)

#### StaleTurnRecoveryHigh
- **含义**: 过时 turn 恢复率过高
- **指标**: `rtc_recovery_stale_turns_recovered_total`
- **阈值**: 每秒 > 0.1 次恢复
- **持续时间**: 5 分钟
- **级别**: warning
- **影响**: 可能存在 turn 处理卡住或超时
- **处理步骤**:
  1. 检查 turn 处理逻辑
  2. 分析超时配置
  3. 检查消费者健康状态
  4. 查看是否有死锁

#### InterruptRateHigh
- **含义**: 中断率过高
- **指标**: `rtc_interrupt_total`
- **阈值**: 每秒 > 1 次中断
- **持续时间**: 5 分钟
- **级别**: warning
- **影响**: 用户体验下降
- **处理步骤**:
  1. 分析中断原因（reason label）
  2. 检查超时配置
  3. 检查客户端连接稳定性
  4. 查看是否有异常客户端行为

#### CheckpointErrors
- **含义**: Checkpoint 操作失败
- **指标**: `rtc_checkpoint_operations_total{status="error"}`
- **阈值**: 5 分钟内有任何错误
- **持续时间**: 2 分钟
- **级别**: warning
- **影响**: 可能导致数据丢失
- **处理步骤**:
  1. 检查错误日志
  2. 验证存储系统状态
  3. 检查 checkpoint 大小限制
  4. 确认数据完整性

#### AttachmentBuildFailures
- **含义**: Attachment 构建失败
- **指标**: `rtc_attachment_operations_total{status="failed"}`
- **阈值**: 5 分钟内有任何失败
- **持续时间**: 2 分钟
- **级别**: warning
- **影响**: LLM 上下文质量下降
- **处理步骤**:
  1. 检查失败的 attachment 类型
  2. 分析错误日志
  3. 检查依赖数据源
  4. 验证 attachment 配置

## 监控指标参考

所有告警规则基于以下 Prometheus 指标：

| 指标名称 | 类型 | 说明 |
|---------|------|------|
| `rtc_auth_security_events_total` | Counter | 安全事件（fail-open、token 重用等） |
| `rtc_auth_failures_total` | Counter | 认证失败 |
| `rtc_config_fallback_total` | Counter | 配置回退到 YAML 默认值 |
| `rtc_circuitbreaker_state` | Gauge | 熔断器当前状态 |
| `rtc_circuitbreaker_state_changes_total` | Counter | 熔断器状态变化 |
| `http_requests_total` | Counter | HTTP 请求总数 |
| `http_request_duration_seconds` | Histogram | HTTP 请求延迟 |
| `rtc_llm_http_requests_total` | Counter | LLM API 请求总数 |
| `rtc_llm_http_request_duration_seconds` | Histogram | LLM API 请求延迟 |
| `rtc_db_queries_total` | Counter | 数据库查询总数 |
| `rtc_db_query_duration_seconds` | Histogram | 数据库查询延迟 |
| `rtc_queue_publish_total` | Counter | 队列发布总数 |
| `rtc_queue_claim_total` | Counter | 队列领取总数 |
| `rtc_queue_wait_duration_seconds` | Histogram | 队列等待时间 |
| `go_goroutines` | Gauge | Goroutine 数量 |
| `rtc_recovery_stale_turns_recovered_total` | Counter | 过时 turn 恢复总数 |
| `rtc_interrupt_total` | Counter | 中断总数 |
| `rtc_checkpoint_operations_total` | Counter | Checkpoint 操作总数 |
| `rtc_attachment_operations_total` | Counter | Attachment 操作总数 |

## 告警响应流程

### 通用流程

1. **确认告警**: 查看 Prometheus 告警详情，确认告警真实有效
2. **评估影响**: 判断告警对用户体验和系统稳定性的影响
3. **通知团队**: 如果是 critical 级别，立即通知 on-call 工程师
4. **诊断问题**: 查看相关日志和指标，定位根本原因
5. **采取措施**: 根据具体情况采取修复措施
6. **记录总结**: 在事故报告中记录处理过程和教训

### 升级策略

- **warning 级别**: 30 分钟内未解决 → 升级为 critical
- **critical 级别**: 15 分钟内未解决 → 通知技术负责人
- **多个 critical**: 同时出现 3 个以上 critical → 启动应急响应流程

## 告警调优建议

1. **定期审查**: 每月审查告警规则，根据实际运行情况调整阈值
2. **避免告警疲劳**: 删除或合并低价值的告警
3. **添加注释**: 为每个告警添加清晰的注释，说明其目的
4. **测试告警**: 使用 `promtool check rules` 验证告警规则语法
5. **文档化**: 为每个告警编写处理文档（runbook）

## 相关文档

- [Prometheus 告警规则官方文档](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/)
- [Grafana 仪表板配置](./grafana-dashboards.md)
- [运维手册](./ops-runbook.md)
