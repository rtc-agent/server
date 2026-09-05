package logger

import (
	"context"
	"runtime/debug"

	"go.uber.org/zap"
)

// SafeGo 启动一个 goroutine，捕获 panic 并记录日志。
// 防止单个 goroutine 的 panic 导致整个进程崩溃。
//
// name 用于标识 goroutine 身份（出现在日志中），便于定位问题。
//
// 用法：
//
//	logger.SafeGo("queue-worker", func() {
//	    w.processSession(ctx, sessionID)
//	})
func SafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				Error(context.Background(), "goroutine panic",
					zap.String("name", name),
					zap.Any("recover", r),
					zap.String("stack", string(debug.Stack())))
			}
		}()
		fn()
	}()
}
