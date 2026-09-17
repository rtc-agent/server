package logger

import (
	"context"
	"runtime/debug"

	"go.uber.org/zap"
)

// SafeGo starts a goroutine, catching panics and logging them.
// Prevents a single goroutine's panic from crashing the entire process.
//
// name identifies the goroutine (appears in logs) for easier troubleshooting.
//
// Usage:
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
