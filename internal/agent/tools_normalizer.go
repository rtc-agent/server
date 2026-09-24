package agent

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// toolCallArgumentsNormalizer normalizes tool arguments before execution.
// Ensures empty/null/whitespace arguments become "{}" so JSON parsing succeeds.
//
// This is a preprocessing middleware that sits outside the logging layer,
// ensuring that:
//  1. All tools receive valid JSON arguments
//  2. The logger sees the normalized arguments (what the tool actually receives)
//  3. Separation of concerns: normalization, logging, and error handling are distinct layers
//
// Wrapper chain (outer to inner):
//
//	errorHandler → normalizer → logger → actual tool
type toolCallArgumentsNormalizer struct {
	inner tool.BaseTool
}

func (n *toolCallArgumentsNormalizer) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return n.inner.Info(ctx)
}

func (n *toolCallArgumentsNormalizer) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	// Normalize empty/null/whitespace to "{}"
	normalized := normalizeToolArguments(argumentsInJSON)

	// Safe type assertion: inner must be InvokableTool
	// (toolCallLogger already validates this, but we check here for safety)
	invokable, ok := n.inner.(tool.InvokableTool)
	if !ok {
		// This should never happen in practice since toolCallLogger validates it,
		// but return an error instead of panicking
		return "", &normalizerError{msg: "inner tool does not implement InvokableTool"}
	}

	return invokable.InvokableRun(ctx, normalized, opts...)
}

// normalizerError is a simple error type for normalizer failures
type normalizerError struct {
	msg string
}

func (e *normalizerError) Error() string {
	return e.msg
}
