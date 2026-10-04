# Admin Server 后端 API 实现总结

## 概述
成功实现了 admin-server 的完整后端 API，包括用户模型、Repository、Usecase、Handler 和路由。

## 实现的文件

### 1. 数据模型层 (Model)
**文件**: `internal/model/user.go`
- User 结构体：包含 id, email, name, avatar_url, password_hash, created_at, updated_at 字段
- BeforeCreate hook：自动生成 UUID v7
- 使用 GORM tag 定义数据库映射
- TableName 方法指定表名为 "users"

### 2. 数据访问层 (Repository)
**文件**: `internal/repo/user_repo.go`
- UserRepo 接口定义：
  - Create(ctx, user) - 创建用户
  - GetByEmail(ctx, email) - 根据邮箱查询用户
  - GetByID(ctx, id) - 根据 ID 查询用户
  - Update(ctx, user) - 更新用户信息
- 实现使用 GORM
- 事务通过 DBFromContext 传递
- 哨兵错误：ErrDuplicateEmail（邮箱重复）
- isDuplicateKeyError 函数：检测 PostgreSQL 唯一约束违反（错误码 23505）

### 3. 业务逻辑层 (Usecase)
**文件**: `internal/usecase/admin_auth.go`
- AdminAuthUsecase 结构体
- 核心方法：
  - Login(ctx, email, password) - 用户登录，验证 bcrypt 密码，签发 JWT
  - GetCurrentUser(ctx, userID) - 获取当前用户信息
  - RefreshToken(ctx, refreshToken) - 刷新 access_token，实现 token 轮换
  - Logout(ctx, refreshToken) - 撤销 refresh_token
  - FindOrCreateUser(ctx, provider, sub, email, name, avatarURL) - OAuth 用户映射
  - HashPassword(password) - 密码哈希工具函数
- 哨兵错误：
  - ErrInvalidCredentials - 无效凭证
  - ErrUserNotFound - 用户不存在
  - ErrInvalidRefreshToken - 无效刷新令牌
  - ErrRefreshTokenRevoked - 刷新令牌已撤销
  - ErrRefreshTokenExpired - 刷新令牌已过期
- 安全特性：
  - 密码使用 bcrypt (cost=12) 哈希存储
  - refresh_token 使用 SHA-256 哈希存储，明文永不持久化
  - token 轮换机制防止重放攻击

### 4. JWT 签发器 (Infrastructure)
**文件**: `internal/infra/auth/admin_jwt.go`
- AdminJWTSigner 结构体
- 支持的算法：RS256, RS384, RS512, ES256, ES384, ES512
- 核心方法：
  - SignAccessToken(userID, email, name) - 签发 access token
  - SignRefreshToken(userID) - 签发 refresh token
  - ParseAccessToken(token) - 验证并解析 access token
  - ParseRefreshToken(token) - 验证并解析 refresh token
  - GetJWKS() - 返回 JWKS 公钥集合
  - AccessTTL() / RefreshTTL() - 获取 TTL 配置
- JWT Claims 包含：sub, iss, aud, email, name, exp, iat, jti
- 支持从 PEM 文件加载密钥对或自动生成临时密钥（开发环境）
- 严格验证：iss, aud, exp, alg

**文件**: `internal/infra/auth/admin_jwt_test.go`
- 完整的单元测试覆盖
- 测试场景：签名、解析、过期、无效 token、JWKS 生成

### 5. HTTP 处理器层 (Handler)
**文件**: `internal/handler/http/admin_auth.go`
- AdminAuthHandler 结构体
- API 端点：
  - POST /api/auth/login - 登录
  - GET /api/auth/me - 获取当前用户（需 JWT 认证）
  - POST /api/auth/refresh - 刷新 Token（需 JWT 认证）
  - POST /api/auth/logout - 登出（需 JWT 认证）
  - GET /.well-known/jwks.json - JWKS 端点
  - GET /health - 健康检查
- JWTAuthMiddleware：Gin 中间件，验证 JWT 并注入 user_id, email, name 到 context
- 错误响应符合 OAuth 2.0 规范（RFC 6749 Section 5.2）
- 日志脱敏：不记录完整的 JWT 或密码

### 6. 配置加载 (Config)
**文件**: `internal/infra/config/admin_config_loader.go`
- LoadAdminConfig(cfgFile) - 加载 admin 配置
- setDefaults() - 设置默认值
- Validate() - 验证配置：
  - 必需的 database.dsn
  - JWT 算法必须是支持的算法之一
  - TTL 必须为正数
  - 服务器端口必须在 1-65535 之间
  - 密钥路径必须同时设置或同时为空

### 7. Cobra 子命令
**文件**: `cmd/admin/admin.go`
- admin 子命令入口
- 加载 admin.yaml 配置
- 初始化数据库（支持自动迁移）
- 初始化 Redis（可选）
- 初始化 JWT 签发器
- 初始化 Repository、Usecase、Handler
- 设置 Gin 路由和中间件
- 启动 HTTP 服务器（支持优雅关闭）
- CORS 中间件

**文件**: `cmd/root.go`
- 注册 admin 子命令到根命令

## 依赖修复

在实现过程中修复了以下依赖问题：

1. **jwx v2 API 变更**
   - `jwk.Read` → `jwk.Parse`（需要先读取数据到内存）
   - `iter.Value()` → `iter.Pair().Value.(jwk.Key)`
   - 修复了 `internal/infra/auth/jwks_client.go`
   - 修复了 `internal/infra/auth/jwks_client_test.go`

2. **jwt/v5 API 变更**
   - `t.Method.Alg()` 现在只返回一个值
   - 修复了 `internal/usecase/token_exchange.go`

3. **Go 版本兼容性**
   - `t.Getenv` 是 Go 1.25+ API
   - 改用 `os.Getenv` 替代

4. **依赖添加**
   - `github.com/lestrrat-go/jwx/v2` - JWKS 支持
   - `golang.org/x/crypto/bcrypt` - 密码哈希

## 数据库迁移

自动迁移支持以下表：
- `users` - 用户表
- `refresh_tokens` - 刷新令牌表

## 配置示例

```yaml
server:
  host: "0.0.0.0"
  port: 8081
  env: "development"

database:
  dsn: "postgres://localhost:5432/rtc_agent_admin?sslmode=disable"
  auto_migrate: true

jwt:
  algorithm: "RS256"
  issuer: "https://admin.example.com"
  audience: "https://rtc.example.com"
  private_key_path: "./etc/keys/admin-private.pem"
  public_key_path: "./etc/keys/admin-public.pem"
  access_token_ttl: 3600      # 1 hour
  refresh_token_ttl: 604800   # 7 days
```

## 使用方法

```bash
# 启动 admin server
go run main.go admin

# 使用自定义配置文件
go run main.go admin --config etc/admin.yaml

# 查看帮助
go run main.go admin --help
```

## 安全特性

1. **密码安全**
   - bcrypt 哈希（cost=12）
   - 密码永不明文存储或记录

2. **JWT 安全**
   - 严格验证 iss, aud, exp, alg
   - 支持密钥轮换（通过 JWKS）
   - Token 轮换机制（refresh_token 使用后自动撤销）

3. **日志脱敏**
   - 不记录完整的 JWT token
   - 不记录密码
   - 敏感信息使用哈希前缀记录

4. **错误处理**
   - 符合 OAuth 2.0 规范的错误响应
   - 不向客户端暴露内部错误细节

## 测试覆盖

- AdminJWTSigner 完整测试
- 签名和解析测试
- 过期 token 测试
- 无效 token 测试
- JWKS 生成测试

## 编译和运行

所有代码已通过编译，测试通过：

```bash
# 编译
go build ./...

# 运行测试
go test ./internal/infra/auth/... -v

# 启动服务
go run main.go admin
```

## 架构分层

严格遵循分层架构：
- Handler → Usecase → Repository
- Handler 不能直接访问 Repository
- 事务通过 context 传递
- 依赖注入清晰

## 后续扩展建议

1. 添加用户角色和权限管理
2. 添加用户列表和搜索 API
3. 添加用户状态管理（激活/禁用）
4. 添加审计日志
5. 添加 rate limiting
6. 添加更多 OAuth provider 支持
7. 添加用户头像上传 API
8. 添加批量操作 API
