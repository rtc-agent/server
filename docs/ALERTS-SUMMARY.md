# 告警规则配置总结

## 已创建文件

1. **prometheus-alerts.yml** - Prometheus 告警规则配置文件
   - 位置: `/Users/leichujun/Workspaces/rtc-agent/server/docs/prometheus-alerts.yml`
   - 包含 5 个告警组，共 18 条告警规则

2. **prometheus-alerts.md** - 告警规则说明文档
   - 位置: `/Users/leichujun/Workspaces/rtc-agent/server/docs/prometheus-alerts.md`
   - 包含所有告警的详细说明、影响分析和处理步骤

## 告警覆盖范围

### 安全性告警 (2 条)
- ✅ Ban 检查 fail-open 事件
- ✅ 认证失败率异常

### 配置管理告警 (3 条)
- ✅ 配置回退到 YAML 默认值
- ✅ DB 熔断器开启 (critical)
- ✅ 熔断器状态抖动

### 性能告警 (5 条)
- ✅ HTTP 高延迟
- ✅ HTTP 高错误率 (critical)
- ✅ LLM API 高延迟
- ✅ LLM API 高错误率 (critical)
- ✅ 数据库查询高延迟

### 资源告警 (4 条)
- ✅ Goroutine 泄漏检测
- ✅ 队列等待时间过长
- ✅ 队列发布错误
- ✅ 数据库查询错误

### 业务逻辑告警 (4 条)
- ✅ 过时 turn 恢复率过高
- ✅ 中断率过高
- ✅ Checkpoint 操作失败
- ✅ Attachment 构建失败

## 配置方法

### 1. 将告警规则导入 Prometheus

编辑 Prometheus 配置文件（通常是 `prometheus.yml`）：

```yaml
rule_files:
  - "/path/to/rtc-agent/server/docs/prometheus-alerts.yml"
```

### 2. 重新加载 Prometheus 配置

```bash
# 方法 1: 发送 SIGHUP 信号
kill -HUP $(pgrep prometheus)

# 方法 2: 使用 HTTP API
curl -X POST http://localhost:9090/-/reload
```

### 3. 验证告警规则已加载

访问 Prometheus UI: `http://localhost:9090/rules`

应该能看到以下告警组：
- rtc-agent-security
- rtc-agent-config
- rtc-agent-performance
- rtc-agent-resources
- rtc-agent-business

## 关键指标来源

所有告警规则基于代码中已实现的 Prometheus 指标：

- `rtc_auth_security_events_total` - 来自 `server/internal/infra/auth/metrics.go`
- `rtc_config_fallback_total` - 来自 `server/pkg/turn-agent/prometheus_metrics.go`
- `rtc_circuitbreaker_state` - 来自 `server/pkg/circuitbreaker/metrics.go`
- `http_requests_total` - 来自 `server/internal/infra/middleware/http_metrics.go`
- `rtc_queue_*` - 来自 `server/pkg/rtc-queue/metrics.go`
- `rtc_db_*` - 来自 `server/internal/infra/db/metrics.go`

## 下一步建议

1. **配置 Alertmanager**: 设置告警通知渠道（Slack、邮件、PagerDuty 等）
2. **编写 Runbook**: 为每个告警编写详细的处理手册
3. **设置值班制度**: 确保 critical 级别告警能及时响应
4. **定期审查**: 每月审查告警规则，根据实际运行情况调整阈值
5. **测试告警**: 在测试环境验证告警规则的有效性

## 注意事项

- 所有告警阈值基于经验值，可能需要根据实际生产环境调整
- 建议先在测试环境验证，再应用到生产环境
- 部分告警（如 GoroutineLeak）的阈值可能需要根据业务规模调整
- 建议配合 Grafana 仪表板一起使用，实现可视化监控
