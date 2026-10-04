# JWT 密钥管理说明

本目录存放 JWT 签名所需的密钥文件。

## 文件说明

| 文件                  | 算法  | 用途       | 权限   |
| --------------------- | ----- | ---------- | ------ |
| rs256-private.pem     | RS256 | JWT 签名   | 600    |
| rs256-public.pem      | RS256 | JWT 验证   | 644    |
| es256-private.pem     | ES256 | JWT 签名   | 600    |
| es256-public.pem      | ES256 | JWT 验证   | 644    |

## 生成密钥

使用项目提供的脚本生成密钥：

```bash
# 生成所有类型（RS256 + ES256）
./scripts/generate-keys.sh all

# 仅生成 RS256
./scripts/generate-keys.sh rs256

# 仅生成 ES256
./scripts/generate-keys.sh es256
```

## 安全存储建议

### 开发环境

开发环境可直接使用本目录存放密钥，但需确保：

1. **不提交到 Git** — 已在 `.gitignore` 中排除 `*.pem` 文件
2. **私钥权限** — 设置为 `chmod 600`，仅当前用户可读
3. **使用脚本生成** — 不要在本地手动创建密钥文件

### 生产环境

生产环境应使用密钥管理服务（KMS）而非本地文件：

| 平台       | 推荐服务                | 说明                    |
| ---------- | ----------------------- | ----------------------- |
| AWS        | AWS KMS / Secrets Manager | 自动轮换，审计日志      |
| 阿里云     | KMS / 凭据管家          | 支持 HSM，合规认证      |
| GCP        | Cloud KMS               | 硬件级安全              |
| 自建       | HashiCorp Vault         | 功能全面，社区版免费    |

## 密钥轮换流程

### 定期轮换

建议每 90 天轮换一次密钥：

1. **生成新密钥对**
   ```bash
   ./scripts/generate-keys.sh all
   ```

2. **部署新密钥**
   - 将新公钥部署到验证服务
   - 将新私钥部署到签名服务

3. **过渡期**
   - 保留旧公钥 24-48 小时（用于验证已签发的 token）
   - 新签发的 token 使用新私钥

4. **清理旧密钥**
   - 确认所有旧 token 已过期后删除旧密钥

### 紧急轮换

当私钥泄露时需立即轮换：

1. **立即撤销所有已签发 token**
2. **生成新密钥对**
3. **部署新密钥**
4. **审计日志，查找异常访问**
5. **通知受影响用户重新登录**

## 算法选择

| 算法  | 密钥长度 | 性能 | 适用场景           |
| ----- | -------- | ---- | ------------------ |
| RS256 | 2048 bit | 较慢 | 传统系统，广泛兼容 |
| ES256 | P-256    | 较快 | 现代系统，推荐首选 |

**推荐**：新系统优先使用 ES256，性能更好，密钥更短。

## 配置示例

在 `config.yaml` 中配置：

```yaml
jwt:
  algorithm: ES256
  private_key_path: etc/keys/es256-private.pem
  public_key_path: etc/keys/es256-public.pem
  issuer: rtc-agent
  access_token_ttl: 15m
  refresh_token_ttl: 7d
```
