package logger

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var log = zap.NewNop()

// DebugMode 表示是否开启了 DEBUG 日志（通过环境变量 DEBUG=true 启用）。
// 开启后会额外输出到文件 logs/debug.log，使用人类可读的 console 编码。
var DebugMode bool

// Init 初始化日志。
// 除 cfg.Level 外，还会检查环境变量 DEBUG：
//   - DEBUG=true / DEBUG=1 → 启用 DebugMode，额外输出到 logs/debug.log（console 编码）
//   - 其他值 → 仅使用 cfg.Level 配置
func Init(level string) {
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "info":
		zapLevel = zapcore.InfoLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	config := zap.Config{
		Level:            zap.NewAtomicLevelAt(zapLevel),
		Encoding:         "json",
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
		EncoderConfig:    encoderConfig,
	}

	l, err := config.Build()
	if err != nil {
		panic(err)
	}

	// 检查 DEBUG 环境变量，启用额外的文件日志（人类可读格式）
	debugEnv := strings.ToLower(os.Getenv("DEBUG"))
	if debugEnv == "true" || debugEnv == "1" || debugEnv == "yes" {
		DebugMode = true

		// 创建 logs 目录（忽略错误——目录已存在也无妨）
		_ = os.MkdirAll("logs", 0o755)

		// 人类可读的 console 编码器
		consoleEncoderConfig := encoderConfig
		consoleEncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		consoleEncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		consoleEncoder := zapcore.NewConsoleEncoder(consoleEncoderConfig)

		// 文件 core：Debug 级别，输出到 logs/debug.log
		debugFile, fileErr := os.OpenFile("logs/debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if fileErr == nil {
			fileCore := zapcore.NewCore(
				consoleEncoder,
				zapcore.AddSync(debugFile),
				zapcore.DebugLevel,
			)

			// 将 stdout (JSON) 和 file (console) 合并为 Tee
			l = zap.New(zapcore.NewTee(l.Core(), fileCore))
		}
	}

	log = l

	// Replace the global logger so zap.L() / zap.S() across the project
	// (including turnloop adapters) point to this configured logger.
	zap.ReplaceGlobals(l)
}

// Sync 刷新日志缓冲
func Sync() {
	if log != nil {
		_ = log.Sync()
	}
}

// Debug 打印调试日志
func Debug(ctx context.Context, msg string, fields ...zap.Field) {
	log.Debug(msg, fields...)
}

// Info 打印信息日志
func Info(ctx context.Context, msg string, fields ...zap.Field) {
	log.Info(msg, fields...)
}

// Warn 打印警告日志
func Warn(ctx context.Context, msg string, fields ...zap.Field) {
	log.Warn(msg, fields...)
}

// Error 打印错误日志
func Error(ctx context.Context, msg string, fields ...zap.Field) {
	log.Error(msg, fields...)
}

// Fatal 打印致命错误日志并退出
func Fatal(ctx context.Context, msg string, fields ...zap.Field) {
	log.Fatal(msg, fields...)
}

// CaptureStack 捕获当前调用栈，返回人类可读的字符串。
// skip 表示跳过的栈帧数（0 = CaptureStack 自身的调用者）。
// 用于在关键入口点记录 "谁调用了这里"。
func CaptureStack(skip int) string {
	var pcs [32]uintptr
	n := runtime.Callers(skip+2, pcs[:]) // +2: skip Callers + CaptureStack
	if n == 0 {
		return "<no stack>"
	}
	frames := runtime.CallersFrames(pcs[:n])
	var sb strings.Builder
	for {
		frame, more := frames.Next()
		// 跳过 runtime 内部帧
		if strings.HasPrefix(frame.Function, "runtime.") {
			if !more {
				break
			}
			continue
		}
		fmt.Fprintf(&sb, "  %s:%d  %s\n", frame.File, frame.Line, frame.Function)
		if !more {
			break
		}
	}
	return sb.String()
}
