package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/callbacks"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/stringutil"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	loggerpkg "github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

//go:embed prompts/session-memory-extract.md
var sessionMemoryExtractPrompt string

// SessionMemoryExtractor extracts session memories.
//
// Trigger conditions (aligned with requirements doc 12-memory-system.md):
//   - Init threshold: context token count >= 10,000
//   - Update threshold (any one of):
//   - token growth >= 5,000 AND tool call count >= 3
//   - token growth >= 5,000 AND last assistant turn has no tool calls (natural conversation breakpoint)
type SessionMemoryExtractor struct {
	chatModel    einomodel.ToolCallingChatModel
	memoryRepo   repo.SessionMemoryRepo
	tokenCounter turnagent.TokenCounterFunc
	logger       turnagent.Logger

	// Configuration
	InitThreshold   int // initialization threshold, default 10000
	UpdateThreshold int // update threshold, default 5000
	MinToolCalls    int // minimum tool call count, default 3

	// noThinkingOptions disables thinking/reasoning (compression tasks don't need reasoning).
	noThinkingOptions []einomodel.Option

	// tokenCallbackHandler tracks token consumption for LLM calls.
	tokenCallbackHandler callbacks.Handler
}

// NewSessionMemoryExtractor creates a SessionMemoryExtractor.
func NewSessionMemoryExtractor(
	chatModel einomodel.ToolCallingChatModel,
	memoryRepo repo.SessionMemoryRepo,
	tokenCounter turnagent.TokenCounterFunc,
	logger turnagent.Logger,
	noThinkingOptions []einomodel.Option,
	tokenCallbackHandler callbacks.Handler,
) *SessionMemoryExtractor {
	if logger == nil {
		logger = loggerpkg.NoopLogger{}
	}
	return &SessionMemoryExtractor{
		chatModel:            chatModel,
		memoryRepo:           memoryRepo,
		tokenCounter:         tokenCounter,
		logger:               logger,
		InitThreshold:        10000,
		UpdateThreshold:      5000,
		MinToolCalls:         3,
		noThinkingOptions:    noThinkingOptions,
		tokenCallbackHandler: tokenCallbackHandler,
	}
}

// ExtractionState tracks extraction state (for tracking position since last extraction).
type ExtractionState struct {
	LastTokenCount   int    // token count at last extraction
	LastMessageCount int    // message count at last extraction
	LastExtractedAt  string // time of last extraction (ISO 8601)
}

// ExtractIfNeeded checks whether extraction is needed and performs it if so.
//
// Returns:
//   - extracted: whether extraction was performed
//   - newState: updated extraction state
//   - err: error information
func (e *SessionMemoryExtractor) ExtractIfNeeded(
	ctx context.Context,
	sessionID uuid.UUID,
	messages []*schema.Message,
	state *ExtractionState,
) (extracted bool, newState *ExtractionState, err error) {
	// Check if chatModel is configured.
	if e.chatModel == nil {
		e.log(ctx, "extractor.skip_chatmodel_nil", map[string]any{
			"session_id": sessionID.String(),
		})
		return false, state, nil
	}

	// Calculate current token count.
	currentTokens, err := e.tokenCounter(ctx, messages)
	if err != nil {
		return false, state, fmt.Errorf("count tokens: %w", err)
	}

	// Check initialization threshold.
	if currentTokens < e.InitThreshold {
		e.log(ctx, "extractor.skip_below_init_threshold", map[string]any{
			"session_id":     sessionID.String(),
			"current_tokens": currentTokens,
			"init_threshold": e.InitThreshold,
		})
		return false, state, nil
	}

	// Calculate token growth.
	tokenGrowth := currentTokens
	if state != nil && state.LastTokenCount > 0 {
		tokenGrowth = currentTokens - state.LastTokenCount
	}

	// Calculate tool call count.
	toolCallCount := countToolCalls(messages, state)

	// Check if update conditions are met.
	// Condition 1: token growth >= 5000 AND tool call count >= 3
	// Condition 2: token growth >= 5000 AND last assistant turn has no tool calls (natural conversation breakpoint)
	shouldExtract := (tokenGrowth >= e.UpdateThreshold && toolCallCount >= e.MinToolCalls) ||
		(tokenGrowth >= e.UpdateThreshold && !hasToolCallInLastAssistant(messages))

	if !shouldExtract {
		e.log(ctx, "extractor.skip_threshold_not_met", map[string]any{
			"session_id":       sessionID.String(),
			"token_growth":     tokenGrowth,
			"tool_call_count":  toolCallCount,
			"update_threshold": e.UpdateThreshold,
			"min_tool_calls":   e.MinToolCalls,
		})
		return false, state, nil
	}

	// Perform extraction.
	e.log(ctx, "extractor.start", map[string]any{
		"session_id":      sessionID.String(),
		"current_tokens":  currentTokens,
		"token_growth":    tokenGrowth,
		"tool_call_count": toolCallCount,
	})

	// Query existing session memories (to avoid duplicate extraction).
	existingMemories, err := e.memoryRepo.ListBySession(ctx, sessionID, 50)
	if err != nil {
		return false, state, fmt.Errorf("list existing memories: %w", err)
	}

	// Call LLM to extract memories.
	newMemories, err := e.extractMemories(ctx, sessionID, messages, existingMemories)
	if err != nil {
		return false, state, fmt.Errorf("extract memories: %w", err)
	}

	// Save new memories.
	if len(newMemories) > 0 {
		for _, mem := range newMemories {
			if err := e.memoryRepo.Create(ctx, mem); err != nil {
				e.log(ctx, "extractor.save_memory_error", map[string]any{
					"session_id": sessionID.String(),
					"error":      err.Error(),
				})
				continue
			}
		}
	}

	// Update state.
	newState = &ExtractionState{
		LastTokenCount:   currentTokens,
		LastMessageCount: len(messages),
	}

	e.log(ctx, "extractor.done", map[string]any{
		"session_id":        sessionID.String(),
		"new_memories":      len(newMemories),
		"existing_memories": len(existingMemories),
	})

	return true, newState, nil
}

// extractMemories calls the LLM to extract memories.
//
// Uses tool calling instead of parsing free text: the LLM must call the saveSessionMemories tool,
// with parameters constrained by JSON schema, completely avoiding the reliability issues of parsing
// JSON from LLM output.
// Uses Stream (not Generate) because the model requires streaming for long operations.
func (e *SessionMemoryExtractor) extractMemories(
	ctx context.Context,
	sessionID uuid.UUID,
	messages []*schema.Message,
	existingMemories []*model.SessionMemory,
) ([]*model.SessionMemory, error) {
	// Set sessionID in context (may already be set by caller, but ensure it's there
	// for the token callback handler to find).
	ctx = turnagent.WithSessionID(ctx, sessionID.String())

	// Initialize eino callbacks so the LLM call's token usage is recorded
	// to Session.TotalTokens via the shared token callback handler.
	if e.tokenCallbackHandler != nil {
		ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, e.tokenCallbackHandler)
	}

	// Build tool.
	extractTool := &saveSessionMemoriesTool{sessionID: sessionID}
	toolInfo, err := extractTool.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("tool info: %w", err)
	}

	// Bind tool to chatModel.
	boundModel, err := e.chatModel.WithTools([]*schema.ToolInfo{toolInfo})
	if err != nil {
		return nil, fmt.Errorf("bind tools: %w", err)
	}

	// Build prompt.
	prompt := e.buildExtractPrompt(messages, existingMemories)
	inputMessages := []*schema.Message{schema.UserMessage(prompt)}

	// Call LLM (using Stream, thinking disabled).
	stream, err := boundModel.Stream(ctx, inputMessages, e.noThinkingOptions...)
	if err != nil {
		return nil, fmt.Errorf("chat model stream: %w", err)
	}

	// Consume stream, merging all chunks (including incrementally fragmented tool calls).
	// ConcatMessageStream closes the stream internally, no need for defer stream.Close().
	resp, err := schema.ConcatMessageStream(stream)
	if err != nil {
		return nil, fmt.Errorf("consume stream: %w", err)
	}

	// Check if LLM called the tool.
	if len(resp.ToolCalls) == 0 {
		e.log(ctx, "extractor.no_tool_call", map[string]any{
			"session_id": sessionID.String(),
		})
		return nil, nil // non-fatal: LLM determined no extraction needed
	}

	// Parse tool call arguments directly (JSON schema constrained, 100% reliable).
	tc := resp.ToolCalls[0]
	if tc.Function.Name != "saveSessionMemories" {
		// LLM called an unexpected tool (hallucination from conversation history).
		// Log a warning and return nil (non-fatal: extraction failed, will retry later).
		e.log(ctx, "extractor.unexpected_tool_call", map[string]any{
			"session_id":    sessionID.String(),
			"tool_name":     tc.Function.Name,
			"expected_tool": "saveSessionMemories",
		})
		return nil, nil
	}

	var args struct {
		Decision  []memoryItem `json:"decision"`
		Context   []memoryItem `json:"context"`
		Progress  []memoryItem `json:"progress"`
		Issue     []memoryItem `json:"issue"`
		Learnings []memoryItem `json:"learnings"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("unmarshal tool args: %w", err)
	}

	// Convert to model.SessionMemory.
	var memories []*model.SessionMemory
	addMemories := func(category string, items []memoryItem) {
		for _, item := range items {
			tokenCount := estimateMemoryTokens(item.Content)
			mem := &model.SessionMemory{
				SessionID:  sessionID,
				Category:   category,
				Title:      item.Title,
				Content:    item.Content,
				Metadata:   item.Metadata,
				TokenCount: &tokenCount,
			}
			memories = append(memories, mem)
		}
	}
	addMemories(model.SessionMemoryCategoryDecision, args.Decision)
	addMemories(model.SessionMemoryCategoryContext, args.Context)
	addMemories(model.SessionMemoryCategoryProgress, args.Progress)
	addMemories(model.SessionMemoryCategoryIssue, args.Issue)
	addMemories(model.SessionMemoryCategoryLearnings, args.Learnings)

	return memories, nil
}

// saveSessionMemoriesTool is the tool for memory extraction.
// The LLM submits extracted memories by calling this tool, with parameters constrained by JSON schema.
type saveSessionMemoriesTool struct {
	sessionID uuid.UUID
}

func (t *saveSessionMemoriesTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	memoryItemSchema := &schema.ParameterInfo{
		Type: schema.Object,
		Desc: "A single memory item",
		SubParams: map[string]*schema.ParameterInfo{
			"title": {
				Type:     schema.String,
				Desc:     "Short title (5-10 words)",
				Required: true,
			},
			"content": {
				Type:     schema.String,
				Desc:     "Detailed description with specifics: file paths, function names, exact values, etc.",
				Required: true,
			},
			"metadata": {
				Type:     schema.Object,
				Desc:     "Optional structured data (e.g. related_files, code_snippets)",
				Required: false,
			},
		},
	}

	return &schema.ToolInfo{
		Name: "saveSessionMemories",
		Desc: "Save extracted session memories. Call this tool with the memories you extracted from the conversation. Only include categories that have NEW information.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"decision":  {Type: schema.Array, Desc: "技术决策: technology choices, design decisions, architecture", Required: false, ElemInfo: memoryItemSchema},
			"context":   {Type: schema.Array, Desc: "当前上下文: what is being worked on, current tasks", Required: false, ElemInfo: memoryItemSchema},
			"progress":  {Type: schema.Array, Desc: "任务进展: completed tasks, current status", Required: false, ElemInfo: memoryItemSchema},
			"issue":     {Type: schema.Array, Desc: "问题与解决: errors, fixes, user corrections", Required: false, ElemInfo: memoryItemSchema},
			"learnings": {Type: schema.Array, Desc: "经验教训: what worked, what didn't, insights", Required: false, ElemInfo: memoryItemSchema},
		}),
	}, nil
}

// InvokableRun satisfies the tool.InvokableTool interface but does not persist
// memories. The actual save happens in extractMemories: the tool's Info() provides
// the JSON schema that constrains the LLM's tool_call output, and extractMemories
// parses the arguments directly from the stream response. InvokableRun is only
// present to satisfy the interface; it validates JSON format and returns a count.
func (t *saveSessionMemoriesTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args struct {
		Decision  []memoryItem `json:"decision"`
		Context   []memoryItem `json:"context"`
		Progress  []memoryItem `json:"progress"`
		Issue     []memoryItem `json:"issue"`
		Learnings []memoryItem `json:"learnings"`
	}
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	total := len(args.Decision) + len(args.Context) + len(args.Progress) + len(args.Issue) + len(args.Learnings)
	return fmt.Sprintf("Successfully saved %d memories.", total), nil
}

// memoryItem is used for tool argument parsing.
type memoryItem struct {
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata"`
}

// buildExtractPrompt builds the extraction prompt.
func (e *SessionMemoryExtractor) buildExtractPrompt(
	messages []*schema.Message,
	existingMemories []*model.SessionMemory,
) string {
	var sb strings.Builder

	// Write prompt template.
	sb.WriteString(sessionMemoryExtractPrompt)
	sb.WriteString("\n\n")

	// Write existing memories (if any).
	if len(existingMemories) > 0 {
		sb.WriteString("# Existing Session Memories\n\n")
		sb.WriteString("The following memories have already been extracted. Do NOT duplicate them, only extract NEW information:\n\n")
		for _, mem := range existingMemories {
			fmt.Fprintf(&sb, "- **[%s]** %s: %s\n", mem.Category, mem.Title, stringutil.TruncateByRune(mem.Content, 200))
		}
		sb.WriteString("\n")
	}

	// Write conversation history.
	sb.WriteString("# Conversation History\n\n")
	sb.WriteString(formatMessagesForMemoryExtract(messages))

	return sb.String()
}

// countToolCalls counts tool calls (since the last extraction).
func countToolCalls(messages []*schema.Message, state *ExtractionState) int {
	startIdx := 0
	if state != nil && state.LastMessageCount > 0 && state.LastMessageCount < len(messages) {
		startIdx = state.LastMessageCount
	}

	count := 0
	for i := startIdx; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == schema.Assistant && len(msg.ToolCalls) > 0 {
			count += len(msg.ToolCalls)
		}
	}
	return count
}

// hasToolCallInLastAssistant checks whether the last assistant message has tool calls.
func hasToolCallInLastAssistant(messages []*schema.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == schema.Assistant {
			return len(messages[i].ToolCalls) > 0
		}
	}
	return false
}

// formatMessagesForMemoryExtract formats messages for memory extraction.
func formatMessagesForMemoryExtract(messages []*schema.Message) string {
	var sb strings.Builder

	for i, msg := range messages {
		role := string(msg.Role)
		if role == "" {
			role = "unknown"
		}

		switch msg.Role {
		case schema.Assistant:
			if msg.Content != "" {
				fmt.Fprintf(&sb, "## %s (turn %d)\n\n", role, i+1)
				sb.WriteString(msg.Content)
				sb.WriteString("\n\n")
			}
			if len(msg.ToolCalls) > 0 {
				sb.WriteString("### Tool Calls\n\n")
				for _, tc := range msg.ToolCalls {
					fmt.Fprintf(&sb, "- **%s** (id: `%s`)\n", tc.Function.Name, tc.ID)
					if tc.Function.Arguments != "" {
						fmt.Fprintf(&sb, "  ```json\n  %s\n  ```\n", tc.Function.Arguments)
					}
				}
				sb.WriteString("\n")
			}

		case schema.Tool:
			fmt.Fprintf(&sb, "## %s (result for %s)\n\n", role, msg.ToolName)
			// Truncate overly long tool results. Uses UTF-8-safe truncation
			// to avoid cutting in the middle of multi-byte characters (e.g. CJK).
			if len(msg.Content) > 2000 {
				fmt.Fprintf(&sb, "%s\n\n", stringutil.TruncateByByte(msg.Content, 2000))
			} else {
				sb.WriteString(msg.Content)
				sb.WriteString("\n\n")
			}

		case schema.User:
			if msg.Content != "" {
				fmt.Fprintf(&sb, "## %s (turn %d)\n\n", role, i+1)
				sb.WriteString(msg.Content)
				sb.WriteString("\n\n")
			}

		case schema.System:
			continue

		default:
			fmt.Fprintf(&sb, "## %s\n\n%s\n\n", role, msg.Content)
		}
	}

	return sb.String()
}

// estimateMemoryTokens estimates token count (using the global TokenCounter).
func estimateMemoryTokens(s string) int {
	return turnagent.CountStringTokens(s)
}

func (e *SessionMemoryExtractor) log(ctx context.Context, event string, fields map[string]any) {
	e.logger.Info(ctx, "[session_memory_extractor] "+event, fields)
}
