-- 001_create_users_table.sql
-- 创建 users 表，用于 admin-server 的用户管理
-- 该表存储管理员账户信息，支持邮箱登录

-- ============================================================
-- 回滚脚本（如需回滚，请先执行 002 的回滚，再执行此脚本）
-- ============================================================
-- DROP TABLE IF EXISTS users;

-- ============================================================
-- 建表语句
-- ============================================================
CREATE TABLE IF NOT EXISTS users (
    -- 主键：UUID v7（由应用层生成，保证时间有序）
    id          UUID            PRIMARY KEY,

    -- 邮箱地址，全局唯一，用于登录
    email       VARCHAR(255)    NOT NULL,

    -- 用户显示名称
    name        VARCHAR(255),

    -- 头像 URL
    avatar_url  VARCHAR(500),

    -- 密码哈希值（bcrypt，cost=12）
    password_hash VARCHAR(255)  NOT NULL,

    -- 记录创建时间（UTC）
    created_at  TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- 记录最后更新时间（UTC）
    updated_at  TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- 邮箱唯一约束
    CONSTRAINT users_email_unique UNIQUE (email)
);

-- ============================================================
-- 索引
-- ============================================================

-- email 索引：加速登录查询（已有 UNIQUE 约束自带索引，此处显式创建以明确意图）
CREATE INDEX IF NOT EXISTS idx_users_email ON users (email);

-- ============================================================
-- 自动更新 updated_at 的触发器
-- ============================================================

-- 创建更新时间的触发器函数（如果尚未存在）
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- 绑定触发器到 users 表
DROP TRIGGER IF EXISTS trigger_users_updated_at ON users;
CREATE TRIGGER trigger_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at_column();
