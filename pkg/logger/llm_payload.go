package logger

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// llmPayloadLog 是独立的 LLM payload 日志记录器。
// 当 log.llm_payload=true 时，将每次 LLM API 调用的完整请求/响应
// 以 JSON Lines 格式写入 logs/llm-payload.log。
//
// 与主 logger（log）分离，原因：
//  1. LLM payload 体积大（完整 prompt/completion），会淹没常规日志
//  2. JSON Lines 格式便于 jq / 日志分析工具处理
//  3. 仅开发环境开启，生产环境保持零开销
//
// 日志轮转：使用 lumberjack 按大小自动切分（默认 100MB/文件，保留 3 个旧文件，
// 最多 7 天，旧文件 gzip 压缩），避免磁盘占满。
var llmPayloadLog *zap.Logger

// LLMPayloadConfig 控制 LLM payload 日志行为。
type LLMPayloadConfig struct {
	Enabled    bool   // 是否启用
	FilePath   string // 日志文件路径，默认 "logs/llm-payload.log"
	MaxSizeMB  int    // 单个文件最大 MB，默认 100
	MaxBackups int    // 保留旧文件数，默认 3
	MaxAgeDays int    // 保留天数，默认 7
	Compress   bool   // 旧文件是否 gzip 压缩，默认 true
	LogLevel   string
}

// InitLLMPayloadLogger 初始化 LLM payload 日志。
// 仅在 cfg.Enabled=true 时创建文件输出，否则 llmPayloadLog 保持 nil。
func InitLLMPayloadLogger(cfg LLMPayloadConfig) {
	if !cfg.Enabled {
		return
	}

	// 默认值
	if cfg.FilePath == "" {
		cfg.FilePath = "logs/llm-payload.log"
	}
	if cfg.MaxSizeMB <= 0 {
		cfg.MaxSizeMB = 10
	}
	if cfg.MaxBackups <= 0 {
		cfg.MaxBackups = 3
	}
	if cfg.MaxAgeDays <= 0 {
		cfg.MaxAgeDays = 7
	}
	// Compress 默认 true（零值时设为 true）
	if !cfg.Compress {
		cfg.Compress = true
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	// lumberjack 轮转 writer
	lj := &lumberjack.Logger{
		Filename:   cfg.FilePath,
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAgeDays,
		Compress:   cfg.Compress,
	}

	// JSON Lines 编码——每行一个完整 JSON 对象，便于 jq 处理
	encoderConfig := zapcore.EncoderConfig{
		TimeKey:       "time",
		LevelKey:      "level",
		NameKey:       "logger",
		MessageKey:    "msg",
		StacktraceKey: "stacktrace",
		LineEnding:    zapcore.DefaultLineEnding,
		EncodeLevel:   zapcore.LowercaseLevelEncoder,
		EncodeTime:    zapcore.ISO8601TimeEncoder,
	}

	logLevel := zapcore.InfoLevel
	if cfg.LogLevel == "debug" {
		logLevel = zapcore.DebugLevel
	}

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(lj),
		//zapcore.DebugLevel, // payload 日志总是 debug 级别，按 msg 字段区分类型
		logLevel,
	)

	llmPayloadLog = zap.New(core)
}

// LLMPayload 返回 LLM payload 日志记录器。
// 如果未启用，返回 nil——调用方应在使用前检查。
func LLMPayload() *zap.Logger {
	return llmPayloadLog
}

// SyncLLMPayload 刷新 LLM payload 日志缓冲。
func SyncLLMPayload() {
	if llmPayloadLog != nil {
		_ = llmPayloadLog.Sync()
	}
}
