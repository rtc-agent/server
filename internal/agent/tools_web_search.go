package agent

import (
	"github.com/cloudwego/eino/components/tool"
	"github.com/rtc-agent/server/pkg/websearch"
	"go.uber.org/zap"
)

// createWebSearchTool creates the web search tool if WebSearchManager is available
func (h *helpers) createWebSearchTool() (tool.InvokableTool, error) {
	if h.deps.WebSearchManager == nil {
		return nil, nil
	}

	// Convert turnagent.Logger to zap.Logger
	// For now, use nop logger as fallback
	logger := zap.NewNop()

	// Pass the embedded description (loaded via go:embed from web_search.md)
	return websearch.NewWebSearchTool(h.deps.WebSearchManager, logger, webSearchDesc)
}
