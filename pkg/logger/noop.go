package logger

import (
	"context"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// NoopLogger implements turnagent.Logger with no-op operations.
// Use this instead of nil checks to eliminate verbose logging guards at call sites.
type NoopLogger struct{}

// Debug is a no-op implementation of the Logger.Debug interface method.
func (NoopLogger) Debug(_ context.Context, _ string, _ map[string]any) {}

// Info is a no-op implementation of the Logger.Info interface method.
func (NoopLogger) Info(_ context.Context, _ string, _ map[string]any) {}

// Warn is a no-op implementation of the Logger.Warn interface method.
func (NoopLogger) Warn(_ context.Context, _ string, _ map[string]any) {}

// Error is a no-op implementation of the Logger.Error interface method.
func (NoopLogger) Error(_ context.Context, _ string, _ map[string]any) {}

// Compile-time assertion that NoopLogger implements turnagent.Logger.
var _ turnagent.Logger = NoopLogger{}
