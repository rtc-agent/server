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
		// Caller is resolved by our smart resolveCaller(), not zap's built-in mechanism.
		DisableCaller: true,
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

// resolveCaller walks the call stack and returns the first non-logger frame.
// It automatically skips logger wrapper layers using heuristics:
//  1. Frames in pkg/logger/ directory (the logger package itself, including GormLogger)
//  2. Frames whose function has a Logger-type receiver (e.g., (*centrifugeLogger).Info)
//
// This works regardless of how many layers of wrapping exist between business code and zap.
// All current adapters (appLogger, workerLogger, centrifugeLogger, GormLogger, defaultLogger)
// are automatically detected by rule 2.
func resolveCaller() zapcore.EntryCaller {
	var pcs [32]uintptr
	// skip=3: runtime.Callers + resolveCaller + the log function (Info/Debug/etc.)
	// so the first frame we inspect is the direct caller of the log function.
	n := runtime.Callers(3, pcs[:])
	if n == 0 {
		return zapcore.EntryCaller{}
	}
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if !isLoggerFrame(frame) {
			return zapcore.EntryCaller{
				Defined:  true,
				File:     formatShortFile(frame.File),
				Line:     frame.Line,
				Function: frame.Function,
			}
		}
		if !more {
			break
		}
	}
	return zapcore.EntryCaller{}
}

// isLoggerFrame returns true if the frame belongs to a logger wrapper layer
// that should be skipped when resolving the real caller.
func isLoggerFrame(f runtime.Frame) bool {
	// 1. Skip frames in pkg/logger/ directory (logger package itself).
	if strings.Contains(f.File, "/pkg/logger/") {
		return true
	}

	// 2. Skip frames whose function has a Logger-type receiver calling a log method.
	// Matches: (*centrifugeLogger).Info, defaultLogger.Warn, (*GormLogger).Trace, etc.
	// Function format: "pkg/path.(*Type).Method" or "pkg/path.Type.Method"
	fn := f.Function
	if idx := strings.LastIndex(fn, "."); idx >= 0 {
		receiver := fn[:idx]
		if strings.Contains(receiver, "Logger") {
			return true
		}
	}

	return false
}

// formatShortFile converts an absolute file path to a short relative form.
// e.g., "/home/user/project/internal/svc/foo.go" → "internal/svc/foo.go"
func formatShortFile(file string) string {
	// Try to find a common project root marker.
	if idx := strings.LastIndex(file, "/server/"); idx >= 0 {
		return file[idx+len("/server/"):]
	}
	// Fallback: last two path segments.
	if idx := strings.LastIndex(file, "/"); idx >= 0 {
		if idx2 := strings.LastIndex(file[:idx], "/"); idx2 >= 0 {
			return file[idx2+1:]
		}
	}
	return file
}

// Debug logs a debug-level message.
func Debug(ctx context.Context, msg string, fields ...zap.Field) {
	if ce := log.Check(zapcore.DebugLevel, msg); ce != nil {
		ce.Caller = resolveCaller()
		ce.Write(append(extractTraceFields(ctx), fields...)...)
	}
}

// Info logs an info-level message.
func Info(ctx context.Context, msg string, fields ...zap.Field) {
	if ce := log.Check(zapcore.InfoLevel, msg); ce != nil {
		ce.Caller = resolveCaller()
		ce.Write(append(extractTraceFields(ctx), fields...)...)
	}
}

// Warn logs a warning-level message.
func Warn(ctx context.Context, msg string, fields ...zap.Field) {
	if ce := log.Check(zapcore.WarnLevel, msg); ce != nil {
		ce.Caller = resolveCaller()
		ce.Write(append(extractTraceFields(ctx), fields...)...)
	}
}

// Error logs an error-level message.
// If any field contains gorm.ErrRecordNotFound, it is downgraded to Warn.
func Error(ctx context.Context, msg string, fields ...zap.Field) {
	if containsErrRecordNotFound(fields) {
		if ce := log.Check(zapcore.WarnLevel, msg); ce != nil {
			ce.Caller = resolveCaller()
			ce.Write(append(extractTraceFields(ctx), fields...)...)
		}
		return
	}
	if ce := log.Check(zapcore.ErrorLevel, msg); ce != nil {
		ce.Caller = resolveCaller()
		ce.Write(append(extractTraceFields(ctx), fields...)...)
	}
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
	if ce := log.Check(zapcore.FatalLevel, msg); ce != nil {
		ce.Caller = resolveCaller()
		ce.Write(append(extractTraceFields(ctx), fields...)...)
	}
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
