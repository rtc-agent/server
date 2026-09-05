// Package agent provides adapter for turn-agent logger.
package agent

import (
	"context"

	"github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.uber.org/zap"
)

// appLogger adapts the application's logger to turn-agent's Logger interface.
type appLogger struct{}

// NewLogger creates a turn-agent Logger that outputs to the application's logger.
func NewLogger() turnagent.Logger {
	return &appLogger{}
}

func (l *appLogger) Debug(ctx context.Context, msg string, fields map[string]any) {
	logger.Debug(ctx, "[turnagent] "+msg, mapToFields(fields)...)
}

func (l *appLogger) Info(ctx context.Context, msg string, fields map[string]any) {
	logger.Info(ctx, "[turnagent] "+msg, mapToFields(fields)...)
}

func (l *appLogger) Warn(ctx context.Context, msg string, fields map[string]any) {
	logger.Warn(ctx, "[turnagent] "+msg, mapToFields(fields)...)
}

func (l *appLogger) Error(ctx context.Context, msg string, fields map[string]any) {
	logger.Error(ctx, "[turnagent] "+msg, mapToFields(fields)...)
}

// mapToFields converts a map[string]any to []zap.Field for the structured logger.
func mapToFields(m map[string]any) []zap.Field {
	fields := make([]zap.Field, 0, len(m))
	for k, v := range m {
		fields = append(fields, zap.Any(k, v))
	}
	return fields
}

// logIfEnabled logs a message using the configured logger, if available.
// This is a convenience wrapper to avoid nil-checking the logger at every call site.
func (h *helpers) logIfEnabled(ctx context.Context, msg string, fields map[string]any) {
	if h.logger != nil {
		h.logger.Info(ctx, msg, fields)
	}
}
