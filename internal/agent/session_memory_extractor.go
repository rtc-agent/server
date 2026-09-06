package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
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
func (e *SessionMemoryExtractor) extractMemories(
	ctx context.Context,
	sessionID uuid.UUID,
	messages []*schema.Message,
	existingMemories []*model.SessionMemory,
) ([]*model.SessionMemory, error) {
	// 构建提示词
	prompt := e.buildExtractPrompt(messages, existingMemories)

	// 调用 LLM（禁用 thinking 以节省 token）
	resp, err := e.chatModel.Generate(ctx, []*schema.Message{
		schema.UserMessage(prompt),
	}, e.noThinkingOptions...)
	if err != nil {
		return nil, fmt.Errorf("chat model generate: %w", err)
	}

	if resp == nil || len(resp.Content) == 0 {
		return nil, fmt.Errorf("chat model returned empty response")
	}

	// 解析响应
	memories, err := e.parseExtractResponse(resp.Content, sessionID)
	if err != nil {
		return nil, fmt.Errorf("parse extract response: %w", err)
	}

	return memories, nil
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

// parseExtractResponse 解析 LLM 响应
func (e *SessionMemoryExtractor) parseExtractResponse(response string, sessionID uuid.UUID) ([]*model.SessionMemory, error) {
	// 提取 JSON 部分（可能在 markdown 代码块中）
	jsonStr := extractJSONFromResponse(response)

	// 解析 JSON
	var result struct {
		Decision  []memoryJSON `json:"decision"`
		Context   []memoryJSON `json:"context"`
		Progress  []memoryJSON `json:"progress"`
		Issue     []memoryJSON `json:"issue"`
		Learnings []memoryJSON `json:"learnings"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, fmt.Errorf("unmarshal json: %w", err)
	}

	// 转换为 model.SessionMemory
	var memories []*model.SessionMemory

	addMemories := func(category string, items []memoryJSON) {
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

	addMemories(model.SessionMemoryCategoryDecision, result.Decision)
	addMemories(model.SessionMemoryCategoryContext, result.Context)
	addMemories(model.SessionMemoryCategoryProgress, result.Progress)
	addMemories(model.SessionMemoryCategoryIssue, result.Issue)
	addMemories(model.SessionMemoryCategoryLearnings, result.Learnings)

	return memories, nil
}

// memoryJSON 用于解析 JSON 响应
type memoryJSON struct {
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Metadata model.JSONB[any] `json:"metadata,omitempty"`
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

// extractJSONFromResponse 从响应中提取 JSON
func extractJSONFromResponse(response string) string {
	// 尝试提取 markdown 代码块中的 JSON
	if idx := strings.Index(response, "```json"); idx != -1 {
		start := idx + 7
		if end := strings.Index(response[start:], "```"); end != -1 {
			return strings.TrimSpace(response[start : start+end])
		}
	}

	// 尝试找到 JSON 对象
	if idx := strings.Index(response, "{"); idx != -1 {
		// 找到最后一个 }
		for i := len(response) - 1; i >= idx; i-- {
			if response[i] == '}' {
				return response[idx : i+1]
			}
		}
	}

	return response
}

// truncateString 截断字符串
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
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

	// Run extraction in background goroutine
	go func() {
		extracted, newState, err := extractor.ExtractIfNeeded(ctx, sessionID, schemaMessages, state)
		if err != nil {
			h.logger.Info(ctx, "[triggerSessionMemoryExtraction] error", map[string]any{
				"session_id": sessionID.String(),
				"error":      err.Error(),
			})
			return
		}
		if extracted {
			h.logger.Info(ctx, "[triggerSessionMemoryExtraction] extracted", map[string]any{
				"session_id": sessionID.String(),
				"new_state":  newState,
			})
		}
	}()
}
