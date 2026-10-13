package agent

import (
	"context"

	einomodel "github.com/cloudwego/eino/components/model"

	"github.com/rtc-agent/server/internal/usecase"
)

// ResolveChatModel resolves the effective ChatModel from dynamic config for background calls.
// Exported for use by RPC handlers and other non-helper code paths.
// Falls back to deps.ChatModel when dynamic config is unavailable or resolution fails.
//
// Deprecated: Handler layer should use usecase.ResolveChatModelForHandler instead.
// This function is kept for backward compatibility and delegates to usecase.
func ResolveChatModel(ctx context.Context, deps *usecase.Dependencies) einomodel.ToolCallingChatModel {
	return usecase.ResolveChatModelForHandler(ctx, deps)
}
