package logger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
	"gorm.io/gorm"
)

var log = zap.NewNop()

// debugMode indicates whether DEBUG logging is enabled (via DEBUG=true environment variable).
// When enabled, additional output is written to logs/debug.log using human-readable console encoding.
// Uses atomic.Bool for concurrency safety.
var debugMode atomic.Bool

// IsDebugMode returns whether the logger is currently in DEBUG mode.
// Replaces direct access to the logger.DebugMode variable for concurrency safety.
func IsDebugMode() bool {
	return debugMode.Load()
}

// Init initializes logging.
// In addition to cfg.Level, it also checks the DEBUG environment variable:
//   - DEBUG=true / DEBUG=1 -> enable debug mode, additional output to logs/debug.log (console encoding)
//   - other values -> only use cfg.Level configuration
//
// If serverLogFile is non-empty, additional JSON-format logs are written to that file (for promtail collection).
func Init(level string, serverLogFile ...string) {
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

	// Check DEBUG environment variable, enable extra file logging (human-readable format).
	debugEnv := strings.ToLower(os.Getenv("DEBUG"))
	if debugEnv == "true" || debugEnv == "1" || debugEnv == "yes" {
		debugMode.Store(true)

		// Create logs directory (ignore error -- directory may already exist).
		_ = os.MkdirAll("logs", 0o755)

		// Human-readable console encoder.
		consoleEncoderConfig := encoderConfig
		consoleEncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		consoleEncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		consoleEncoder := zapcore.NewConsoleEncoder(consoleEncoderConfig)

		// Lumberjack rotating writer: 100MB/file, keep 3 old files, max 7 days, gzip compression.
		debugLJ := &lumberjack.Logger{
			Filename:   "logs/debug.log",
			MaxSize:    100, // MB
			MaxBackups: 3,
			MaxAge:     7, // days
			Compress:   true,
		}

		// File core: Debug level, output to logs/debug.log.
		fileCore := zapcore.NewCore(
			consoleEncoder,
			zapcore.AddSync(debugLJ),
			zapcore.DebugLevel,
		)

		// Merge stdout (JSON) and file (console) into a Tee.
		l = zap.New(zapcore.NewTee(l.Core(), fileCore))
	}

	// Server log file (JSON format, for promtail collection).
	if len(serverLogFile) > 0 && serverLogFile[0] != "" {
		logPath := serverLogFile[0]
		// Ensure directory exists (LastIndex returns -1 when path has no separator, skip MkdirAll).
		if idx := strings.LastIndex(logPath, "/"); idx > 0 {
			if dir := logPath[:idx]; dir != "" {
				_ = os.MkdirAll(dir, 0o755)
			}
		}
		// Lumberjack rotating writer: 100MB/file, keep 3 old files, max 7 days, gzip compression.
		serverLJ := &lumberjack.Logger{
			Filename:   logPath,
			MaxSize:    100, // MB
			MaxBackups: 3,
			MaxAge:     7, // days
			Compress:   true,
		}
		// JSON encoder (same as stdout).
		jsonEncoder := zapcore.NewJSONEncoder(encoderConfig)
		fileCore := zapcore.NewCore(
			jsonEncoder,
			zapcore.AddSync(serverLJ),
			zapLevel,
		)
		l = zap.New(zapcore.NewTee(l.Core(), fileCore))
	}

	log = l

	// Replace the global logger so zap.L() / zap.S() across the project
	// (including turnloop adapters) point to this configured logger.
	zap.ReplaceGlobals(l)
}

// Sync flushes the log buffer.
func Sync() {
	if log != nil {
		_ = log.Sync()
	}
}

// extractTraceFields extracts OpenTelemetry trace information from the context.
// Returns fields containing trace_id and span_id (if a valid span exists).
func extractTraceFields(ctx context.Context) []zap.Field {
	if ctx == nil {
		return nil
	}
	span := trace.SpanFromContext(ctx)
	if span == nil || !span.SpanContext().IsValid() {
		return nil
	}
	sc := span.SpanContext()
	return []zap.Field{
		zap.String("trace_id", sc.TraceID().String()),
		zap.String("span_id", sc.SpanID().String()),
	}
}

// Debug logs a debug-level message.
func Debug(ctx context.Context, msg string, fields ...zap.Field) {
	log.Debug(msg, append(extractTraceFields(ctx), fields...)...)
}

// Info logs an info-level message.
func Info(ctx context.Context, msg string, fields ...zap.Field) {
	log.Info(msg, append(extractTraceFields(ctx), fields...)...)
}

// Warn logs a warning-level message.
func Warn(ctx context.Context, msg string, fields ...zap.Field) {
	log.Warn(msg, append(extractTraceFields(ctx), fields...)...)
}

// Error logs an error-level message.
// If any field contains gorm.ErrRecordNotFound, it is downgraded to Warn.
func Error(ctx context.Context, msg string, fields ...zap.Field) {
	if containsErrRecordNotFound(fields) {
		log.Warn(msg, append(extractTraceFields(ctx), fields...)...)
		return
	}
	log.Error(msg, append(extractTraceFields(ctx), fields...)...)
}

// containsErrRecordNotFound checks if any zap.Field contains gorm.ErrRecordNotFound.
func containsErrRecordNotFound(fields []zap.Field) bool {
	for _, f := range fields {
		if f.Interface != nil {
			if err, ok := f.Interface.(error); ok && errors.Is(err, gorm.ErrRecordNotFound) {
				return true
			}
		}
	}
	return false
}

// Fatal logs a fatal-level message and exits.
func Fatal(ctx context.Context, msg string, fields ...zap.Field) {
	log.Fatal(msg, append(extractTraceFields(ctx), fields...)...)
}

// CaptureStack captures the current call stack, returning a human-readable string.
// skip indicates the number of stack frames to skip (0 = caller of CaptureStack).
// Used at key entry points to record "who called here".
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
		// Skip internal runtime frames.
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
