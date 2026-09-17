package config

import (
	"os"
	"regexp"
)

// expandEnvVars 展开配置中的敏感字段的环境变量引用。
// 仅对包含敏感信息的字段（密码、密钥、DSN）生效。
func expandEnvVars(cfg *Config) {
	// 数据库连接字符串（可能包含密码）
	cfg.Database.DSN = expandEnvRef(cfg.Database.DSN)

	// Redis 密码
	cfg.Redis.Password = expandEnvRef(cfg.Redis.Password)

	// Asynq Redis 密码
	cfg.Asynq.RedisPassword = expandEnvRef(cfg.Asynq.RedisPassword)

	// LLM API 密钥
	cfg.LLM.APIKey = expandEnvRef(cfg.LLM.APIKey)

	// OAuth2 Provider 密钥
	cfg.Providers.Mock.ClientSecret = expandEnvRef(cfg.Providers.Mock.ClientSecret)
	cfg.Providers.GitHub.ClientSecret = expandEnvRef(cfg.Providers.GitHub.ClientSecret)
	cfg.Providers.Google.ClientSecret = expandEnvRef(cfg.Providers.Google.ClientSecret)

	// Auth 密钥
	cfg.Auth.JWTSecret = expandEnvRef(cfg.Auth.JWTSecret)

	// Metrics 密码
	cfg.Metrics.Password = expandEnvRef(cfg.Metrics.Password)
	cfg.Debug.Password = expandEnvRef(cfg.Debug.Password)
}

// envRefPattern 匹配 ${VAR_NAME} 形式的环境变量引用。
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvRef 将字符串中的 ${VAR} 替换为对应环境变量值。
// 若环境变量未设置则替换为空字符串。不含 ${...} 的字符串原样返回。
func expandEnvRef(s string) string {
	return envRefPattern.ReplaceAllStringFunc(s, func(match string) string {
		key := envRefPattern.FindStringSubmatch(match)[1]
		val, ok := os.LookupEnv(key)
		if !ok {
			return ""
		}
		return val
	})
}

// ExpandEnvRef 导出供测试使用。
func ExpandEnvRef(s string) string {
	return expandEnvRef(s)
}
