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

// GormLogger 将 GORM 的日志转发到 zap logger
type GormLogger struct {
	ctx                    context.Context
	ignoreRecordNotFound   bool
	slowThreshold          time.Duration
}

// NewGormLogger 创建 GORM logger 适配器
// ignoreRecordNotFound: 是否忽略 ErrRecordNotFound 错误（不输出日志）
// slowThreshold: 慢查询阈值，超过此时间的查询会被记录为 Warn
func NewGormLogger(ignoreRecordNotFound bool, slowThreshold time.Duration) *GormLogger {
	return &GormLogger{
		ctx:                  context.Background(),
		ignoreRecordNotFound: ignoreRecordNotFound,
		slowThreshold:        slowThreshold,
	}
}

// LogMode 实现 gormlogger.Interface
func (l *GormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	// 返回新的 logger 实例（GORM 要求不可变）
	return &GormLogger{
		ctx:                  l.ctx,
		ignoreRecordNotFound: l.ignoreRecordNotFound,
		slowThreshold:        l.slowThreshold,
	}
}

// Info 实现 gormlogger.Interface
func (l *GormLogger) Info(ctx context.Context, msg string, data ...interface{}) {
	Info(ctx, fmt.Sprintf("[gorm] "+msg, data...))
}

// Warn 实现 gormlogger.Interface
func (l *GormLogger) Warn(ctx context.Context, msg string, data ...interface{}) {
	Warn(ctx, fmt.Sprintf("[gorm] "+msg, data...))
}

// Error 实现 gormlogger.Interface
func (l *GormLogger) Error(ctx context.Context, msg string, data ...interface{}) {
	// 检查是否是 ErrRecordNotFound 且需要忽略
	if l.ignoreRecordNotFound && len(data) > 0 {
		if err, ok := data[0].(error); ok && errors.Is(err, gorm.ErrRecordNotFound) {
			// 忽略 ErrRecordNotFound，不输出日志
			return
		}
	}

	Error(ctx, fmt.Sprintf("[gorm] "+msg, data...), zap.Error(data[0].(error)))
}

// Trace 实现 gormlogger.Interface
// 用于记录 SQL 执行信息
func (l *GormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)
	sql, rows := fc()

	// 构建日志字段
	fields := []zap.Field{
		zap.Duration("elapsed", elapsed),
		zap.String("sql", sql),
		zap.Int64("rows", rows),
	}

	// 根据错误和耗时选择日志级别
	if err != nil {
		// 检查是否是 ErrRecordNotFound 且需要忽略
		if l.ignoreRecordNotFound && errors.Is(err, gorm.ErrRecordNotFound) {
			// 忽略 ErrRecordNotFound，不输出日志
			return
		}

		Error(ctx, "[gorm] SQL error", append(fields, zap.Error(err))...)
		return
	}

	// 慢查询警告
	if l.slowThreshold > 0 && elapsed > l.slowThreshold {
		Warn(ctx, "[gorm] Slow query", fields...)
		return
	}

	// 普通查询信息（Debug 级别）
	Debug(ctx, "[gorm] SQL executed", fields...)
}
