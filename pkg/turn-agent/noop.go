package turnagent

import "context"

// noopLogger implements Logger with no-op operations.
// Used as a default when no logger is provided in Config, eliminating
// nil checks at every call site.
type noopLogger struct{}

func (noopLogger) Debug(_ context.Context, _ string, _ map[string]any) {}
func (noopLogger) Info(_ context.Context, _ string, _ map[string]any)  {}
func (noopLogger) Warn(_ context.Context, _ string, _ map[string]any)  {}
func (noopLogger) Error(_ context.Context, _ string, _ map[string]any) {}

// Compile-time assertion that noopLogger implements Logger.
var _ Logger = noopLogger{}
