package logger

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// llmPayloadLog is a separate LLM payload logger.
// When log.llm_payload=true, writes the full LLM API request/response
// in JSON Lines format to logs/llm-payload.log.
//
// Separated from the main logger (log) because:
//  1. LLM payloads are large (full prompt/completion) and would overwhelm regular logs.
//  2. JSON Lines format is easy to process with jq and log analysis tools.
//  3. Only enabled in development; zero overhead in production.
//
// Log rotation: uses lumberjack for automatic size-based splitting (default 100MB/file, keep 3 old files,
// max 7 days, old files gzip compressed) to prevent disk exhaustion.
var llmPayloadLog *zap.Logger

// LLMPayloadConfig controls LLM payload logging behavior.
type LLMPayloadConfig struct {
	Enabled    bool   // Whether to enable.
	FilePath   string // Log file path, default "logs/llm-payload.log".
	MaxSizeMB  int    // Max MB per file, default 100.
	MaxBackups int    // Number of old files to retain, default 3.
	MaxAgeDays int    // Retention in days, default 7.
	Compress   bool   // Whether to gzip compress old files, default true.
	LogLevel   string
}

// InitLLMPayloadLogger initializes the LLM payload logger.
// Only creates file output when cfg.Enabled=true; otherwise llmPayloadLog remains nil.
func InitLLMPayloadLogger(cfg LLMPayloadConfig) {
	if !cfg.Enabled {
		return
	}

	// Defaults.
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
	// Compress defaults to true (set to true when zero value).
	if !cfg.Compress {
		cfg.Compress = true
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	// Lumberjack rotating writer.
	lj := &lumberjack.Logger{
		Filename:   cfg.FilePath,
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAgeDays,
		Compress:   cfg.Compress,
	}

	// JSON Lines encoding -- one complete JSON object per line, easy for jq processing.
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
		//zapcore.DebugLevel, // payload log is always debug level; distinguish types via msg field
		logLevel,
	)

	llmPayloadLog = zap.New(core)
}

// LLMPayload returns the LLM payload logger.
// Returns nil if not enabled -- callers should check before use.
func LLMPayload() *zap.Logger {
	return llmPayloadLog
}

// SyncLLMPayload flushes the LLM payload log buffer.
func SyncLLMPayload() {
	if llmPayloadLog != nil {
		_ = llmPayloadLog.Sync()
	}
}
