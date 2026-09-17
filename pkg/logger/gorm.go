package logger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// GormLogger forwards GORM logs to the zap logger.
type GormLogger struct {
	ctx                  context.Context
	ignoreRecordNotFound bool
	slowThreshold        time.Duration
}

// NewGormLogger creates a GORM logger adapter.
// ignoreRecordNotFound: whether to ignore ErrRecordNotFound errors (suppress log output).
// slowThreshold: slow query threshold; queries exceeding this duration are logged as warnings.
func NewGormLogger(ignoreRecordNotFound bool, slowThreshold time.Duration) *GormLogger {
	return &GormLogger{
		ctx:                  context.Background(),
		ignoreRecordNotFound: ignoreRecordNotFound,
		slowThreshold:        slowThreshold,
	}
}

// LogMode implements gormlogger.Interface.
func (l *GormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	// Return a new logger instance (GORM requires immutability).
	return &GormLogger{
		ctx:                  l.ctx,
		ignoreRecordNotFound: l.ignoreRecordNotFound,
		slowThreshold:        l.slowThreshold,
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
	// Check if this is ErrRecordNotFound and should be ignored.
	if l.ignoreRecordNotFound && len(data) > 0 {
		if err, ok := data[0].(error); ok && errors.Is(err, gorm.ErrRecordNotFound) {
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

	// Build log fields.
	fields := []zap.Field{
		zap.Duration("elapsed", elapsed),
		zap.String("sql", sql),
		zap.Int64("rows", rows),
	}

	// Choose log level based on error and duration.
	if err != nil {
		// Check if this is ErrRecordNotFound and should be ignored.
		if l.ignoreRecordNotFound && errors.Is(err, gorm.ErrRecordNotFound) {
			// Ignore ErrRecordNotFound, suppress log output.
			return
		}

		Error(ctx, "[gorm] SQL error", append(fields, zap.Error(err))...)
		return
	}

	// Slow query warning.
	if l.slowThreshold > 0 && elapsed > l.slowThreshold {
		Warn(ctx, "[gorm] Slow query", fields...)
		return
	}

	// Normal query info (Debug level).
	Debug(ctx, "[gorm] SQL executed", fields...)
}
