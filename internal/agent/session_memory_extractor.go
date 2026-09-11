package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.uber.org/zap"
)

//go:embed prompts/session-memory-extract.md
var sessionMemoryExtractPrompt string

// SessionMemoryExtractor 提取会话记忆
//
// 触发条件（对齐需求文档 12-memory-system.md）：
// - 初始化阈值：上下文 token 数 >= 10,000
// - 更新阈值（满足其一）：
//   - token 增长 >= 5,000 且 tool call 数量 >= 3
//   - token 增长 >= 5,000 且 最后一轮 assistant 没有 tool call（自然对话断点）
type SessionMemoryExtractor struct {
	chatModel    einomodel.ToolCallingChatModel
	memoryRepo   repo.SessionMemoryRepo
	tokenCounter turnagent.TokenCounterFunc
	logger       turnagent.Logger

	// 配置
	InitThreshold   int // 初始化阈值，默认 10000
	UpdateThreshold int // 更新阈值，默认 5000
	MinToolCalls    int // 最小 tool call 数量，默认 3

	// noThinkingOptions 禁用 thinking 的选项（压缩任务不需要推理）
	noThinkingOptions []einomodel.Option
}

// NewSessionMemoryExtractor 创建 SessionMemoryExtractor
func NewSessionMemoryExtractor(
	chatModel einomodel.ToolCallingChatModel,
	memoryRepo repo.SessionMemoryRepo,
	tokenCounter turnagent.TokenCounterFunc,
	logger turnagent.Logger,
	noThinkingOptions []einomodel.Option,
) *SessionMemoryExtractor {
	return &SessionMemoryExtractor{
		chatModel:         chatModel,
		memoryRepo:        memoryRepo,
		tokenCounter:      tokenCounter,
		logger:            logger,
		InitThreshold:     10000,
		UpdateThreshold:   5000,
		MinToolCalls:      3,
		noThinkingOptions: noThinkingOptions,
	}
}

// ExtractionState 提取状态（用于跟踪上次提取的位置）
type ExtractionState struct {
	LastTokenCount   int    // 上次提取时的 token 数
	LastMessageCount int    // 上次提取时的消息数
	LastExtractedAt  string // 上次提取的时间（ISO 8601）
}

// ExtractIfNeeded 检查是否需要提取，如果需要则执行提取
//
// 返回值：
// - extracted: 是否执行了提取
// - newState: 更新后的提取状态
// - err: 错误信息
func (e *SessionMemoryExtractor) ExtractIfNeeded(
	ctx context.Context,
	sessionID uuid.UUID,
	messages []*schema.Message,
	state *ExtractionState,
) (extracted bool, newState *ExtractionState, err error) {
	// 检查 chatModel 是否已配置
	if e.chatModel == nil {
		e.log(ctx, "extractor.skip_chatmodel_nil", map[string]any{
			"session_id": sessionID.String(),
		})
		return false, state, nil
	}

	// 计算当前 token 数
	currentTokens, err := e.tokenCounter(ctx, messages)
	if err != nil {
		return false, state, fmt.Errorf("count tokens: %w", err)
	}

	// 检查初始化阈值
	if currentTokens < e.InitThreshold {
		e.log(ctx, "extractor.skip_below_init_threshold", map[string]any{
			"session_id":     sessionID.String(),
			"current_tokens": currentTokens,
			"init_threshold": e.InitThreshold,
		})
		return false, state, nil
	}

	// 计算 token 增长
	tokenGrowth := currentTokens
	if state != nil && state.LastTokenCount > 0 {
		tokenGrowth = currentTokens - state.LastTokenCount
	}

	// 计算 tool call 数量
	toolCallCount := countToolCalls(messages, state)

	// 检查是否满足更新条件
	// 条件 1: token 增长 >= 5000 且 tool call 数量 >= 3
	// 条件 2: token 增长 >= 5000 且 最后一轮 assistant 没有 tool call（自然对话断点）
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

	// 执行提取
	e.log(ctx, "extractor.start", map[string]any{
		"session_id":      sessionID.String(),
		"current_tokens":  currentTokens,
		"token_growth":    tokenGrowth,
		"tool_call_count": toolCallCount,
	})

	// 查询现有的 session memories（用于避免重复提取）
	existingMemories, err := e.memoryRepo.ListBySession(ctx, sessionID, 50)
	if err != nil {
		return false, state, fmt.Errorf("list existing memories: %w", err)
	}

	// 调用 LLM 提取记忆
	newMemories, err := e.extractMemories(ctx, sessionID, messages, existingMemories)
	if err != nil {
		return false, state, fmt.Errorf("extract memories: %w", err)
	}

	// 保存新记忆
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

	// 更新状态
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

// extractMemories 调用 LLM 提取记忆
//
// 使用 tool calling 而非解析自由文本：LLM 必须调用 save_session_memories tool，
// 参数由 JSON schema 约束，彻底避免了从 LLM 输出中解析 JSON 的可靠性问题。
// 使用 Stream（而非 Generate）因为模型要求长时间操作必须流式。
func (e *SessionMemoryExtractor) extractMemories(
	ctx context.Context,
	sessionID uuid.UUID,
	messages []*schema.Message,
	existingMemories []*model.SessionMemory,
) ([]*model.SessionMemory, error) {
	// 构建 tool
	extractTool := &saveSessionMemoriesTool{sessionID: sessionID}
	toolInfo, err := extractTool.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("tool info: %w", err)
	}

	// 绑定 tool 到 chatModel
	boundModel, err := e.chatModel.WithTools([]*schema.ToolInfo{toolInfo})
	if err != nil {
		return nil, fmt.Errorf("bind tools: %w", err)
	}

	// 构建提示词
	prompt := e.buildExtractPrompt(messages, existingMemories)
	inputMessages := []*schema.Message{schema.UserMessage(prompt)}

	// 调用 LLM（使用 Stream，禁用 thinking）
	stream, err := boundModel.Stream(ctx, inputMessages, e.noThinkingOptions...)
	if err != nil {
		return nil, fmt.Errorf("chat model stream: %w", err)
	}

	// 消费流，合并所有 chunk（包括增量分片的 tool calls）
	// ConcatMessageStream 内部会关闭 stream，不需要 defer stream.Close()
	resp, err := schema.ConcatMessageStream(stream)
	if err != nil {
		return nil, fmt.Errorf("consume stream: %w", err)
	}

	// 检查 LLM 是否调用了 tool
	if len(resp.ToolCalls) == 0 {
		e.log(ctx, "extractor.no_tool_call", map[string]any{
			"session_id": sessionID.String(),
		})
		return nil, nil // 非致命：LLM 认为无需提取
	}

	// 直接解析 tool call 参数（JSON schema 约束，100% 可靠）
	tc := resp.ToolCalls[0]
	if tc.Function.Name != "save_session_memories" {
		return nil, fmt.Errorf("unexpected tool call: %s", tc.Function.Name)
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

	// 转换为 model.SessionMemory
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

// saveSessionMemoriesTool 是 memory extraction 专用的 tool。
// LLM 通过调用此 tool 提交提取的记忆，参数由 JSON schema 约束。
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
		Name: "save_session_memories",
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

// memoryItem 用于 tool 参数解析
type memoryItem struct {
	Title    string           `json:"title"`
	Content  string           `json:"content"`
	Metadata model.JSONB[any] `json:"metadata"`
}

// buildExtractPrompt 构建提取提示词
func (e *SessionMemoryExtractor) buildExtractPrompt(
	messages []*schema.Message,
	existingMemories []*model.SessionMemory,
) string {
	var sb strings.Builder

	// 写入提示词模板
	sb.WriteString(sessionMemoryExtractPrompt)
	sb.WriteString("\n\n")

	// 写入现有记忆（如果有）
	if len(existingMemories) > 0 {
		sb.WriteString("# Existing Session Memories\n\n")
		sb.WriteString("The following memories have already been extracted. Do NOT duplicate them, only extract NEW information:\n\n")
		for _, mem := range existingMemories {
			fmt.Fprintf(&sb, "- **[%s]** %s: %s\n", mem.Category, mem.Title, truncateString(mem.Content, 200))
		}
		sb.WriteString("\n")
	}

	// 写入对话历史
	sb.WriteString("# Conversation History\n\n")
	sb.WriteString(formatMessagesForMemoryExtract(messages))

	return sb.String()
}

// countToolCalls 计算 tool call 数量（从上次提取之后）
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

// hasToolCallInLastAssistant 检查最后一条 assistant 消息是否有 tool call
func hasToolCallInLastAssistant(messages []*schema.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == schema.Assistant {
			return len(messages[i].ToolCalls) > 0
		}
	}
	return false
}

// formatMessagesForMemoryExtract 格式化消息用于记忆提取
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
			// 截断过长的工具结果
			content := msg.Content
			if len(content) > 2000 {
				content = content[:2000] + "... [truncated]"
			}
			sb.WriteString(content)
			sb.WriteString("\n\n")

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

// truncateString 截断字符串（按 rune 截断，避免在多字节字符中间截断）
func truncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// estimateMemoryTokens 估算 token 数（重命名以避免与 summarize.go 中的 estimateTokens 冲突）
func estimateMemoryTokens(s string) int {
	return len(s) / 4 // 4 chars per token
}

func (e *SessionMemoryExtractor) log(ctx context.Context, event string, fields map[string]any) {
	if e.logger != nil {
		e.logger.Info(ctx, "[session_memory_extractor] "+event, fields)
		return
	}
	// 转换为 zap fields
	zapFields := make([]zap.Field, 0, len(fields))
	for k, v := range fields {
		zapFields = append(zapFields, zap.Any(k, v))
	}
	logger.Info(ctx, "[session_memory_extractor] "+event, zapFields...)
}

// triggerSessionMemoryExtraction triggers background session memory extraction.
// This is called from data_context.go after loading messages.
func (h *helpers) triggerSessionMemoryExtraction(ctx context.Context, sessionID uuid.UUID, messages []*turnagent.Message) {
	// Early return if ChatModel is not configured (LLM disabled)
	if h.deps.ChatModel == nil {
		h.logger.Info(ctx, "[triggerSessionMemoryExtraction] skip: ChatModel is nil (LLM not configured)", map[string]any{
			"session_id": sessionID.String(),
		})
		return
	}

	// Convert turnagent.Message to schema.Message for the extractor
	schemaMessages := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		schemaMsg := &schema.Message{
			Role: schema.RoleType(msg.Role),
		}
		if msg.Content != "" {
			schemaMsg.Content = msg.Content
		}
		// Convert tool calls if present
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				schemaMsg.ToolCalls = append(schemaMsg.ToolCalls, schema.ToolCall{
					ID: tc.ID,
					Function: schema.FunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		}
		schemaMessages = append(schemaMessages, schemaMsg)
	}

	// Create extractor
	extractor := NewSessionMemoryExtractor(
		h.deps.ChatModel,
		h.deps.SessionMemoryRepo,
		func(ctx context.Context, msgs []*schema.Message) (int, error) {
			// Simple token estimation: 4 chars per token
			total := 0
			for _, msg := range msgs {
				total += len(msg.Content) / 4
			}
			return total, nil
		},
		h.logger,
		h.noThinkingOptions(), // 禁用 thinking，节省 token
	)

	// Load extraction state from session metadata (if available)
	// For now, use nil state (will be enhanced later to persist state)
	var state *ExtractionState

	// Run extraction in background goroutine.
	// Use context.WithoutCancel to detach from the parent context, so extraction
	// continues even if the turn completes and the parent context is canceled.
	go func() {
		bgCtx := context.WithoutCancel(ctx)
		extracted, newState, err := extractor.ExtractIfNeeded(bgCtx, sessionID, schemaMessages, state)
		if err != nil {
			h.logger.Info(bgCtx, "[triggerSessionMemoryExtraction] error", map[string]any{
				"session_id": sessionID.String(),
				"error":      err.Error(),
			})
			return
		}
		if extracted {
			h.logger.Info(bgCtx, "[triggerSessionMemoryExtraction] extracted", map[string]any{
				"session_id": sessionID.String(),
				"new_state":  newState,
			})
		}
	}()
}
