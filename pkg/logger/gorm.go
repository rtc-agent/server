package logger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// maxSQLLength is the maximum length of SQL statement to log (in bytes).
// SQL longer than this will be truncated to avoid excessive log output.
const maxSQLLength = 500

// truncateSQL safely truncates a SQL string to maxLen bytes without breaking
// multi-byte UTF-8 characters. If truncated, appends a marker.
func truncateSQL(sql string, maxLen int) string {
	if len(sql) <= maxLen {
		return sql
	}
	// Find a safe cut point that doesn't break a UTF-8 character.
	// Scan backward from maxLen to find a valid UTF-8 rune boundary.
	cutPoint := maxLen
	for cutPoint > 0 && !utf8.RuneStart(sql[cutPoint]) {
		cutPoint--
	}
	return sql[:cutPoint] + "... (truncated)"
}

// GormLogger forwards GORM logs to the zap logger.
type GormLogger struct {
	ctx                  context.Context
	ignoreRecordNotFound bool
	slowThreshold        time.Duration
	onSlowQuery          func(operation string, elapsed time.Duration) // optional callback for slow queries
}

// NewGormLogger creates a GORM logger adapter.
// ignoreRecordNotFound: whether to ignore ErrRecordNotFound errors (suppress log output).
// slowThreshold: slow query threshold; queries exceeding this duration are logged as warnings.
// onSlowQuery: optional callback invoked when a slow query is detected. Used for metrics collection.
func NewGormLogger(ignoreRecordNotFound bool, slowThreshold time.Duration, onSlowQuery ...func(operation string, elapsed time.Duration)) *GormLogger {
	var callback func(string, time.Duration)
	if len(onSlowQuery) > 0 {
		callback = onSlowQuery[0]
	}
	return &GormLogger{
		ctx:                  context.Background(),
		ignoreRecordNotFound: ignoreRecordNotFound,
		slowThreshold:        slowThreshold,
		onSlowQuery:          callback,
	}
}

// shouldIgnoreError checks if the error should be ignored (e.g., ErrRecordNotFound).
// This consolidates the ignore logic to avoid duplication across Error() and Trace() methods.
func (l *GormLogger) shouldIgnoreError(err error) bool {
	return l.ignoreRecordNotFound && errors.Is(err, gorm.ErrRecordNotFound)
}

// LogMode implements gormlogger.Interface.
func (l *GormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	// Return a new logger instance (GORM requires immutability).
	return &GormLogger{
		ctx:                  l.ctx,
		ignoreRecordNotFound: l.ignoreRecordNotFound,
		slowThreshold:        l.slowThreshold,
		onSlowQuery:          l.onSlowQuery,
	}
}

// Info implements gormlogger.Interface.
func (l *GormLogger) Info(ctx context.Context, msg string, data ...interface{}) {
	formatted := fmt.Sprintf(msg, data...)
	Info(ctx, "[gorm] info", zap.String("detail", formatted))
}

// Warn implements gormlogger.Interface.
func (l *GormLogger) Warn(ctx context.Context, msg string, data ...interface{}) {
	formatted := fmt.Sprintf(msg, data...)
	Warn(ctx, "[gorm] warn", zap.String("detail", formatted))
}

// Error implements gormlogger.Interface.
func (l *GormLogger) Error(ctx context.Context, msg string, data ...interface{}) {
	// Check if this error should be ignored (e.g., ErrRecordNotFound).
	if len(data) > 0 {
		if err, ok := data[0].(error); ok && l.shouldIgnoreError(err) {
			// Ignore ErrRecordNotFound, suppress log output.
			return
		}
	}

	formatted := fmt.Sprintf(msg, data...)
	fields := []zap.Field{zap.String("detail", formatted)}
	if len(data) > 0 {
		if err, ok := data[0].(error); ok {
			fields = append(fields, zap.Error(err))
		}
	}
	Error(ctx, "[gorm] error", fields...)
}

// Trace implements gormlogger.Interface.
// Used to record SQL execution information.
func (l *GormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)
	sql, rows := fc()

	// Truncate SQL if too long to avoid excessive log output.
	sqlToLog := truncateSQL(sql, maxSQLLength)

	// Build log fields.
	fields := []zap.Field{
		zap.Duration("elapsed", elapsed),
		zap.String("sql", sqlToLog),
		zap.Int64("rows", rows),
	}

	// Choose log level based on error and duration.
	if err != nil {
		// Check if this error should be ignored (e.g., ErrRecordNotFound).
		if l.shouldIgnoreError(err) {
			// Ignore ErrRecordNotFound, suppress log output.
			return
		}

		Error(ctx, "[gorm] SQL error", append(fields, zap.Error(err))...)
		return
	}

	// Slow query warning.
	if l.slowThreshold > 0 && elapsed > l.slowThreshold {
		Warn(ctx, "[gorm] Slow query", fields...)
		// Invoke slow query callback for metrics collection.
		if l.onSlowQuery != nil {
			operation := extractSQLOperation(sql)
			l.onSlowQuery(operation, elapsed)
		}
		return
	}

	// Normal query info (Debug level).
	Debug(ctx, "[gorm] SQL executed", fields...)
}

// extractSQLOperation extracts the SQL operation type from a SQL statement.
// Returns one of: "select", "insert", "update", "delete", "raw".
func extractSQLOperation(sql string) string {
	// Normalize to uppercase for case-insensitive matching.
	upper := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(upper, "SELECT"):
		return "select"
	case strings.HasPrefix(upper, "INSERT"):
		return "insert"
	case strings.HasPrefix(upper, "UPDATE"):
		return "update"
	case strings.HasPrefix(upper, "DELETE"):
		return "delete"
	default:
		return "raw"
	}
}
