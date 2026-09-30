# rtc-oss3 测试指南

## 概述

rtc-oss3 对象存储子系统提供单元测试和集成测试。单元测试无需外部依赖，集成测试需要本地 Docker 环境中的 MinIO 服务。

## 测试类型

### 1. 单元测试（无需外部依赖）

```bash
cd server
go test ./pkg/rtc-oss3/... -v
```

**覆盖内容**：
- 错误映射函数
- 磁盘满检测
- 数据结构验证

### 2. 集成测试（需要 MinIO）

#### 前置条件

1. **启动本地 Docker 环境**

```bash
cd server
docker compose up -d minio
```

2. **等待 MinIO 就绪**

```bash
# 检查 MinIO 健康状态
docker compose ps minio
# 或等待健康检查通过
docker compose exec minio mc ready local
```

3. **配置环境变量**（可选，有默认值）

```bash
export MINIO_ENDPOINT=localhost:29000
export MINIO_ACCESS_KEY=minioadmin
export MINIO_SECRET_KEY=minioadmin
export INTEGRATION_TEST=1
```

#### 运行集成测试

```bash
cd server
INTEGRATION_TEST=1 go test ./pkg/rtc-oss3/... -v -run TestMinIOBackendIntegration
```

**测试内容**：
- HealthCheck：验证 MinIO 连接
- PutObject：上传对象
- GetObject：下载对象并验证内容
- HeadObject：获取对象元数据
- ListObjects：列出对象
- CopyObject：复制对象
- DeleteObject：删除对象
- DeleteObjects：批量删除
- MultipartUpload：完整分片上传流程
- AbortMultipartUpload：中止分片上传
- ErrorMapping：错误处理验证

## 可观测性验证

### Prometheus 指标采集

MinIO 指标已配置在 Prometheus 中：

```bash
# 访问 Prometheus UI
open http://localhost:29090

# 查看 MinIO 指标
# 在 PromQL 中输入：
# minio_s3_requests_total
# minio_node_disk_used_bytes
# up{job="minio"}
```

### Grafana Dashboard

MinIO 概览 Dashboard 已配置：

```bash
# 访问 Grafana
open http://localhost:23001
# 默认账号: admin / admin

# Dashboard 路径: MinIO Object Storage
# UID: minio-overview
```

**Dashboard 面板**：
- MinIO Status（在线/离线）
- Online/Offline Nodes
- Total Buckets
- S3 Request Rate（按 API 分类）
- S3 Request Errors（按错误码分类）
- S3 Request Latency（p95）
- Disk Usage（使用量/总量）
- Bucket Usage（表格视图）

### 日志采集

MinIO 日志通过 Promtail 自动采集到 Loki：

```bash
# 在 Grafana 中查看 MinIO 日志
# 使用 Loki 数据源，查询：
# {service="minio"}
```

### 告警规则

MinIO 相关告警已配置：

- **MinIODown**: MinIO 服务不可达（critical）
- **MinIODiskUsageHigh**: 磁盘使用率 > 85%（warning）
- **MinIODiskUsageCritical**: 磁盘使用率 > 95%（critical）
- **MinIOHighErrorRate**: S3 请求错误率 > 5%（warning）
- **MinIOHighLatency**: S3 请求 p95 延迟 > 5s（warning）
- **MinIOOfflineNodes**: 有节点离线（critical）
- **MinIOQuotaExceeded**: Bucket 使用量超过 10GB（warning）

查看告警状态：

```bash
# 访问 Alertmanager UI
open http://localhost:29093
```

## 完整测试流程

### 1. 启动完整 Docker 环境

```bash
cd server
docker compose up -d
```

### 2. 等待所有服务就绪

```bash
# 检查服务状态
docker compose ps

# 等待关键服务健康
docker compose exec redis redis-cli ping
docker compose exec minio mc ready local
```

### 3. 配置 OSS3（可选）

如果要在 Server 中启用 OSS3：

```bash
# 生成 AES-256 密钥
export OSS3_SESSION_TOKEN_KEY=$(openssl rand -hex 32)

# 配置环境变量
export STORAGE_BACKEND=minio

# 重启 Server
docker compose restart server-1 server-2
```

### 4. 运行集成测试

```bash
INTEGRATION_TEST=1 go test ./pkg/rtc-oss3/... -v
```

### 5. 验证可观测性

- **Prometheus**: http://localhost:29090 → Status → Targets（确认 minio 状态为 UP）
- **Grafana**: http://localhost:23001 → Dashboards → MinIO Object Storage
- **Alertmanager**: http://localhost:29093 → Alerts（确认无活跃告警）

### 6. 清理

```bash
# 停止所有服务
docker compose down

# 停止并删除数据卷（完全清理）
docker compose down -v
```

## 故障排查

### MinIO 连接失败

```bash
# 检查 MinIO 容器状态
docker compose ps minio

# 查看 MinIO 日志
docker compose logs minio

# 测试连接
curl http://localhost:29000/minio/health/live
```

### Prometheus 无法采集 MinIO 指标

```bash
# 检查 Prometheus targets
curl http://localhost:29090/api/v1/targets | jq '.data.activeTargets[] | select(.labels.job=="minio")'

# 手动测试指标端点
curl http://localhost:29000/minio/v2/metrics/cluster
```

### Grafana Dashboard 无数据

1. 确认 Prometheus 数据源已配置
2. 确认 MinIO job 在 Prometheus 中状态为 UP
3. 检查 Dashboard 时间范围（右上角）

## 性能测试

运行大规模上传/下载测试：

```bash
# 使用 MinIO Client (mc)
docker compose exec minio mc alias set local http://minio:9000 minioadmin minioadmin

# 上传大文件
docker compose exec minio mc cp /data/large-file.bin local/rtc-agent/test/

# 下载测试
docker compose exec minio mc cp local/rtc-agent/test/large-file.bin /tmp/
```

## CI/CD 集成

在 CI 环境中运行集成测试：

```yaml
# GitHub Actions 示例
- name: Start MinIO
  run: |
    docker compose up -d minio
    sleep 10

- name: Run Integration Tests
  env:
    INTEGRATION_TEST: 1
    MINIO_ENDPOINT: localhost:29000
    MINIO_ACCESS_KEY: minioadmin
    MINIO_SECRET_KEY: minioadmin
  run: go test ./pkg/rtc-oss3/... -v -run TestMinIOBackendIntegration
```

## 下一步

完成以下任务后，可以进行完整 E2E 测试：

1. 实现 1C 数据访问层（FileRepo、MultipartUploadRepo、TemporaryCredentialRepo）
2. 实现 1D 业务逻辑层（OSS3Usecase、凭证生成、配额管理）
3. 实现 1E S3 协议层（SigV4 验证、HTTP Handler）
4. 实现 1G DI 集成（ServiceContext、Wire、路由注册）

然后可以使用 AWS SDK 或 S3 SDK 进行完整的端到端测试。
