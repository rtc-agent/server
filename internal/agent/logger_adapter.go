// Package agent provides adapter for turn-agent logger.
package agent

import (
	"context"

	"github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// appLogger adapts the application's logger to turn-agent's Logger interface.
type appLogger struct{}

// NewLogger creates a turn-agent Logger that outputs to the application's logger.
func NewLogger() turnagent.Logger {
	return &appLogger{}
}

func (l *appLogger) Debug(ctx context.Context, msg string, fields map[string]any) {
	logger.Debug("[turnagent] %s: %v", msg, fields)
}

func (l *appLogger) Info(ctx context.Context, msg string, fields map[string]any) {
	logger.Info("[turnagent] %s: %v", msg, fields)
}

func (l *appLogger) Warn(ctx context.Context, msg string, fields map[string]any) {
	logger.Warn("[turnagent] %s: %v", msg, fields)
}

func (l *appLogger) Error(ctx context.Context, msg string, fields map[string]any) {
	logger.Error("[turnagent] %s: %v", msg, fields)
}
