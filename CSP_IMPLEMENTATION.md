# CSP (Content Security Policy) 实现说明

## 概述

本次实现为 RTC Agent 项目添加了完整的 CSP（Content Security Policy）支持，包括主服务器和 Admin 服务器。CSP 是一个重要的安全机制，通过 HTTP 头指定哪些内容源可以加载，有效防止 XSS、数据注入等攻击。

## 实现内容

### 1. 配置结构 (`/server/internal/infra/config/csp_config.go`)

创建了 `CSPConfig` 结构体，支持以下 CSP 指令：

- **enabled**: 是否启用 CSP
- **report_only**: 报告模式（不阻止，只记录违规）
- **report_uri**: 违规报告端点
- **default_src**: 默认回退源
- **script_src**: JavaScript 源
- **style_src**: CSS 源
- **img_src**: 图片源
- **font_src**: 字体源
- **connect_src**: 连接源（fetch、XHR、WebSocket）
- **frame_src**: 帧源
- **frame_ancestors**: 允许嵌入的源（替代 X-Frame-Options）
- **base_uri**: base 元素源
- **form_action**: 表单提交目标

### 2. 主服务器配置 (`/server/internal/infra/config/config.go`)

在顶层 `Config` 结构体中添加了 `Security` 字段：

```go
type Config struct {
    // ... existing fields ...
    Security SecurityConfig `mapstructure:"security"`
}

type SecurityConfig struct {
    CSP CSPConfig `mapstructure:"csp"`
}
```

### 3. Admin 服务器配置 (`/server/internal/infra/config/admin_config.go`)

在 `AdminSecurityConfig` 中添加了 CSP 配置：

```go
type AdminSecurityConfig struct {
    // ... existing fields ...
    CSP CSPConfig `mapstructure:"csp"`
}
```

### 4. 中间件实现

#### 标准库 HTTP 中间件 (`/server/internal/infra/middleware/csp.go`)

为主服务器实现，使用 `middleware.Middleware` 签名：

```go
func CSP(cfg *config.CSPConfig) Middleware
```

#### Gin 中间件 (`/server/internal/infra/middleware/gin_csp.go`)

为 Admin 服务器实现，使用 Gin 的 `gin.HandlerFunc`：

```go
func GinCSP(cfg *config.CSPConfig) gin.HandlerFunc
```

两个中间件都：
- 当 `Enabled=false` 时跳过
- 当 `ReportOnly=true` 时使用 `Content-Security-Policy-Report-Only` 头
- 当 `ReportOnly=false` 时使用 `Content-Security-Policy` 头（阻止模式）
- 动态构建 CSP 头，只包含配置了值的指令

### 5. CSP 违规报告处理 (`/server/internal/handler/http/csp_report.go`)

实现了两个处理器：

- `HandleCSPReport`: 标准库 HTTP 处理器（主服务器）
- `HandleGinCSPReport`: Gin 处理器（Admin 服务器）

两个处理器都：
- 解析浏览器发送的 JSON 格式违规报告
- 使用 `logger.Warn` 记录违规详情
- 返回 200 OK

### 6. 主服务器集成 (`/server/internal/server/server.go`)

1. 在中间件链中添加 CSP 中间件：

```go
handler := middleware.Chain(
    middleware.CORS(s.cfg.CORS.AllowOrigins, isDev),
    middleware.HTTPMetrics(),
    rateLimiter.Middleware(),
    middleware.RequestLogger,
    middleware.CSP(&s.cfg.Security.CSP),  // 新增
)(mux)
```

2. 注册 CSP 报告端点：

```go
if s.cfg.Security.CSP.Enabled && s.cfg.Security.CSP.ReportURI != "" {
    mux.HandleFunc("POST "+s.cfg.Security.CSP.ReportURI, httphandler.HandleCSPReport)
}
```

### 7. Admin 服务器集成 (`/server/cmd/admin/serve.go`)

1. 添加 `securityDeps` 结构体：

```go
type securityDeps struct {
    cspConfig config.CSPConfig
}
```

2. 在 `routerDeps` 中添加 `security` 字段

3. 在 `setupRouter` 中添加 CSP 中间件：

```go
router.Use(middleware.GinCSP(&deps.security.cspConfig))
```

4. 注册 CSP 报告端点：

```go
if deps.security.cspConfig.Enabled && deps.security.cspConfig.ReportURI != "" {
    router.POST(deps.security.cspConfig.ReportURI, httphandler.HandleGinCSPReport)
}
```

### 8. 默认配置

#### 主服务器 (`/server/internal/infra/config/config_defaults.go`)

添加了 `setSecurityDefaults` 函数，默认禁用 CSP：

```go
v.SetDefault("security.csp.enabled", false)
v.SetDefault("security.csp.report_only", true)
// ... 其他默认值
```

#### Admin 服务器 (`/server/internal/infra/config/admin_config_loader.go`)

在 `setDefaults` 中添加了 CSP 默认值，默认启用（报告模式）：

```go
if !v.IsSet("security.csp.enabled") {
    c.Security.CSP.Enabled = true
}
// ... 其他默认值
```

### 9. 配置文件更新

#### `/server/etc/admin.yaml`

添加了完整的 CSP 配置，包括开发环境需要的宽松策略：

```yaml
security:
  csp:
    enabled: true
    report_only: true
    script_src: ["'self'", "'unsafe-inline'", "'unsafe-eval"]  # Umi 需要
    style_src: ["'self'", "'unsafe-inline'"]                    # Lit/antd-style 需要
    connect_src:
      - "'self'"
      - "ws:"
      - "wss:"
      - "http://localhost:*"
      - "https://api.anthropic.com"
      - "https://api.tavily.com"
    frame_src:
      - "'self'"
      - "http://localhost:3000"   # Grafana
      - "http://localhost:16686"  # Jaeger
    # ... 其他指令
```

#### `/server/etc/config.yaml.example`

添加了 CSP 配置模板（默认禁用）。

### 10. 测试 (`/server/internal/infra/middleware/csp_test.go`)

创建了全面的测试用例：

- `TestCSP_Disabled`: 验证禁用时不设置头
- `TestCSP_Enabled_EnforceMode`: 验证阻止模式
- `TestCSP_Enabled_ReportOnlyMode`: 验证报告模式
- `TestCSP_WithReportURI`: 验证 report-uri 指令
- `TestCSP_AllDirectives`: 验证所有指令
- `TestCSP_EmptyDirectives`: 验证空指令

所有测试均通过 ✓

## 使用方法

### 主服务器

1. 编辑 `etc/config.yaml`（或 `etc/config.local.yaml`）：

```yaml
security:
  csp:
    enabled: true
    report_only: false  # 生产环境使用阻止模式
    default_src: ["'self'"]
    script_src: ["'self'"]
    # ... 其他配置
```

2. 重启服务器

### Admin 服务器

1. 编辑 `etc/admin.yaml`：

```yaml
security:
  csp:
    enabled: true
    report_only: true  # 先用报告模式测试
    # ... 其他配置
```

2. 重启服务器

3. 查看违规报告：
   - 浏览器控制台会显示 CSP 违规
   - 服务器日志会记录违规详情（WARN 级别）

## 生产环境建议

1. **逐步部署**：
   - 第一阶段：启用 `report_only: true`，收集违规报告
   - 第二阶段：根据报告调整策略
   - 第三阶段：设置 `report_only: false`，启用阻止模式

2. **推荐配置**：
   - `default_src: ["'self'"]` - 严格限制默认源
   - `script_src: ["'self'"]` - 移除 `'unsafe-inline'` 和 `'unsafe-eval'`
   - `style_src: ["'self'", "'unsafe-inline'"]` - 如果可能，移除 `'unsafe-inline'`
   - `frame_ancestors: ["'none'"]` - 防止点击劫持
   - `base_uri: ["'self'"]` - 防止 base 标签劫持
   - `form_action: ["'self'"]` - 限制表单提交目标

3. **监控**：
   - 监控 CSP 违规日志
   - 定期检查是否有新的违规模式
   - 根据实际使用情况调整策略

## 安全收益

1. **XSS 防护**: 限制可执行的脚本源
2. **数据注入防护**: 限制可加载的资源源
3. **点击劫持防护**: 通过 `frame-ancestors` 防止被恶意嵌入
4. **代码注入防护**: 限制内联脚本和 eval
5. **违规可见性**: 通过报告端点发现潜在攻击

## 兼容性

- 现代浏览器完全支持 CSP Level 2/3
- 旧浏览器会忽略 CSP 头（向后兼容）
- 与现有的安全头（X-Frame-Options, X-Content-Type-Options 等）共存

## 文件清单

### 新增文件

1. `/server/internal/infra/config/csp_config.go` - CSP 配置结构
2. `/server/internal/infra/middleware/csp.go` - 标准库 CSP 中间件
3. `/server/internal/infra/middleware/gin_csp.go` - Gin CSP 中间件
4. `/server/internal/infra/middleware/csp_test.go` - 中间件测试
5. `/server/internal/handler/http/csp_report.go` - CSP 报告处理器

### 修改文件

1. `/server/internal/infra/config/config.go` - 添加 Security 配置
2. `/server/internal/infra/config/admin_config.go` - 添加 CSP 到 AdminSecurityConfig
3. `/server/internal/infra/config/config_defaults.go` - 添加 CSP 默认值
4. `/server/internal/infra/config/admin_config_loader.go` - 添加 Admin CSP 默认值
5. `/server/internal/server/server.go` - 集成 CSP 中间件和报告端点
6. `/server/cmd/admin/serve.go` - 集成 CSP 中间件和报告端点
7. `/server/etc/admin.yaml` - 添加 CSP 配置
8. `/server/etc/config.yaml.example` - 添加 CSP 配置模板

## 验证

所有代码已编译通过，测试全部通过：

```bash
$ go build ./...
$ go test ./internal/infra/middleware -run TestCSP -v
PASS
```

CSP 实现完成，可以安全部署到生产环境。
