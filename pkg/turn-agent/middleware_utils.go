// Package turnagent — middleware_utils.go
//
// Shared utilities for ChatModelAgentMiddleware implementations.

package turnagent

import "context"

// middlewareLog dispatches a log call to the appropriate Logger method based on
// the level string. Used internally to keep call sites compact — each call site
// specifies the level as a string ("debug"/"info"/"warn"/"error") rather than
// selecting a Logger method explicitly. Unknown levels fall back to Logger.Info.
//
// Shared by summarize_middleware.go and merge_assistant_middleware.go to avoid
// code duplication.
func middlewareLog(logger Logger, ctx context.Context, level, msg string, attrs map[string]any) {
	if logger == nil {
		return
	}
	switch level {
	case "debug":
		logger.Debug(ctx, msg, attrs)
	case "info":
		logger.Info(ctx, msg, attrs)
	case "warn":
		logger.Warn(ctx, msg, attrs)
	case "error":
		logger.Error(ctx, msg, attrs)
	default:
		logger.Info(ctx, msg, attrs)
	}
}
