-- 002_create_oauth_users_table.sql
-- 创建 oauth_users 表，用于 RTC Agent Server 的 OAuth 用户管理
-- 该表存储通过第三方 OAuth 提供者（Google、GitHub 等）登录的用户信息

-- ============================================================
-- 回滚脚本
-- ============================================================
-- DROP TABLE IF EXISTS oauth_users;

-- ============================================================
-- 建表语句
-- ============================================================
CREATE TABLE IF NOT EXISTS oauth_users (
    -- 主键：UUID v7（由应用层生成，保证时间有序）
    id          UUID            PRIMARY KEY,

    -- OAuth 提供者标识（如 google、github、wechat 等）
    provider    VARCHAR(100)    NOT NULL,

    -- OAuth 提供者的用户唯一标识（subject）
    sub         VARCHAR(255)    NOT NULL,

    -- 用户邮箱（可选，部分提供者可能不返回）
    email       VARCHAR(255),

    -- 用户显示名称
    name        VARCHAR(255),

    -- 头像 URL
    avatar_url  VARCHAR(500),

    -- 记录创建时间（UTC）
    created_at  TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- 记录最后更新时间（UTC）
    updated_at  TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- 联合唯一约束：同一 provider 下 sub 必须唯一
    CONSTRAINT oauth_users_provider_sub_unique UNIQUE (provider, sub)
);

-- ============================================================
-- 索引
-- ============================================================

-- provider + sub 联合索引：加速 OAuth 登录时的用户查找
CREATE INDEX IF NOT EXISTS idx_oauth_users_provider_sub ON oauth_users (provider, sub);

-- email 索引：支持通过邮箱查找关联的 OAuth 账户
CREATE INDEX IF NOT EXISTS idx_oauth_users_email ON oauth_users (email);

-- ============================================================
-- 自动更新 updated_at 的触发器
-- ============================================================

-- 复用 001 迁移中创建的 update_updated_at_column() 函数

-- 绑定触发器到 oauth_users 表
DROP TRIGGER IF EXISTS trigger_oauth_users_updated_at ON oauth_users;
CREATE TRIGGER trigger_oauth_users_updated_at
    BEFORE UPDATE ON oauth_users
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at_column();
