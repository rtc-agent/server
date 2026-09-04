package turnagent

import "context"

// LogLevel represents the severity of a log message.
type LogLevel int

const (
	// LogLevelDebug is for detailed information (LLM input/output, state transitions).
	LogLevelDebug LogLevel = iota
	// LogLevelInfo is for general operational information (turn start/end, events).
	LogLevelInfo
	// LogLevelWarn is for recoverable issues or fallbacks.
	LogLevelWarn
	// LogLevelError is for failures that affect the session.
	LogLevelError
)

// String returns the string representation of the log level.
func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "DEBUG"
	case LogLevelInfo:
		return "INFO"
	case LogLevelWarn:
		return "WARN"
	case LogLevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// Logger is the interface for structured logging within turn-agent.
// Implementations should be thread-safe as Logger may be called from multiple goroutines.
//
// All methods receive a context for extracting trace IDs, session IDs, and other
// contextual information. The fields map contains structured data that should be
// serialized according to the implementation's format (JSON, logfmt, etc.).
//
// Example implementation using log/slog:
//
//	type SlogLogger struct {
//	    logger *slog.Logger
//	}
//
//	func (l *SlogLogger) Info(ctx context.Context, msg string, fields map[string]any) {
//	    args := make([]any, 0, len(fields)*2)
//	    for k, v := range fields {
//	        args = append(args, k, v)
//	    }
//	    l.logger.InfoContext(ctx, msg, args...)
//	}
type Logger interface {
	// Debug logs a message at Debug level.
	// Used for detailed state transitions and diagnostic information.
	Debug(ctx context.Context, msg string, fields map[string]any)

	// Info logs a message at Info level.
	// Used for turn start/end, interrupt/resume events, and cancel events.
	Info(ctx context.Context, msg string, fields map[string]any)

	// Warn logs a message at Warn level.
	// Used for recoverable errors and fallbacks.
	Warn(ctx context.Context, msg string, fields map[string]any)

	// Error logs a message at Error level.
	// Used for failures that affect the turn or indicate bugs.
	Error(ctx context.Context, msg string, fields map[string]any)
}
