-- initial_admin.sql
-- 初始化管理员账户
-- 密码：admin123（bcrypt，cost=12）
--
-- 注意：此脚本应在执行完 001_create_users_table.sql 后运行
-- 生产环境请务必修改默认密码！

-- ============================================================
-- 回滚脚本
-- ============================================================
-- DELETE FROM users WHERE email = 'admin@example.com';

-- ============================================================
-- 插入初始管理员
-- ============================================================

-- UUID v7 示例值（实际部署时由应用层生成时间有序的 UUID v7）
-- 此处使用时间戳固定的 UUID 以保证幂等性
INSERT INTO users (id, email, name, password_hash)
VALUES (
    '01926a0e-4000-7000-8000-000000000001',   -- UUID v7 格式示例
    'admin@example.com',
    'Administrator',
    '$2b$12$ZVfdJRTIVwuxcIKeAGG5CeQX91GXX8JjoacVYArzONLwm05b.LqXy'
)
ON CONFLICT (email) DO NOTHING;
