package logger

import (
	"context"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// NoopLogger implements turnagent.Logger with no-op operations.
// Use this instead of nil checks to eliminate verbose logging guards at call sites.
type NoopLogger struct{}

func (NoopLogger) Debug(_ context.Context, _ string, _ map[string]any) {}
func (NoopLogger) Info(_ context.Context, _ string, _ map[string]any)  {}
func (NoopLogger) Warn(_ context.Context, _ string, _ map[string]any)  {}
func (NoopLogger) Error(_ context.Context, _ string, _ map[string]any) {}

// Compile-time assertion that NoopLogger implements turnagent.Logger.
var _ turnagent.Logger = NoopLogger{}
