package turnagent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/schema"
	"github.com/pkoukk/tiktoken-go"
)

// estimatedImageChars is the fixed character estimate for image content.
// Approximately 250 tokens (1000 chars / 4).
const estimatedImageChars = 1000

// =============================================================================
// TokenCounter interface
// =============================================================================

// TokenCounter defines the interface for token counting.
// Two implementations are provided:
//   - HeuristicTokenCounter: Fast, ~4 chars/token estimate
//   - TiktokenTokenCounter: Precise, uses cl100k_base encoding
type TokenCounter interface {
	// CountTokens returns the token count for the given text.
	CountTokens(text string) int
	// CountMessageTokens returns the token count for a schema.Message.
	CountMessageTokens(msg *schema.Message) int
}

// =============================================================================
// HeuristicTokenCounter
// =============================================================================

// HeuristicTokenCounter uses the fast ~4 chars/token heuristic.
// This is the current behavior and is accurate for English text.
type HeuristicTokenCounter struct{}

// CountTokens estimates token count using the ~4 chars/token heuristic.
func (h *HeuristicTokenCounter) CountTokens(text string) int {
	return len(text) / 4
}

// CountMessageTokens estimates token count for a single message.
func (h *HeuristicTokenCounter) CountMessageTokens(msg *schema.Message) int {
	if msg == nil {
		return 0
	}

	var charCount int

	// 主内容
	charCount += len(msg.Content)
	charCount += len(msg.ReasoningContent)

	// UserInputMultiContent（用户输入多模态内容）
	for _, part := range msg.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			charCount += len(part.Text)
		}
		// 图片等多模态内容按固定 token 估算（约 250 tokens）
		if part.Type == schema.ChatMessagePartTypeImageURL {
			charCount += estimatedImageChars
		}
	}

	// AssistantGenMultiContent（模型输出多模态内容）
	for _, part := range msg.AssistantGenMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			charCount += len(part.Text)
		} else if part.Type == schema.ChatMessagePartTypeReasoning && part.Reasoning != nil {
			charCount += len(part.Reasoning.Text)
		}
	}

	// ToolCalls（工具调用）
	for _, tc := range msg.ToolCalls {
		charCount += len(tc.Function.Name)
		charCount += len(tc.Function.Arguments)
		charCount += len(tc.ID) // tool call ID
	}

	// Tool 结果消息
	if msg.Role == schema.Tool {
		charCount += len(msg.ToolCallID)
		charCount += len(msg.ToolName)
	}

	return charCount / 4 // 4 chars per token
}

// =============================================================================
// TiktokenTokenCounter
// =============================================================================

// TiktokenTokenCounter uses tiktoken-go for precise token counting.
// Uses cl100k_base encoding (GPT-4, GPT-3.5-turbo).
// For Claude models, accuracy is within 5-15% of actual token count.
type TiktokenTokenCounter struct {
	encoding *tiktoken.Tiktoken
	once     sync.Once
	err      error
}

// NewTiktokenTokenCounter creates a new TiktokenTokenCounter.
// The encoding is lazily initialized on first use.
func NewTiktokenTokenCounter() *TiktokenTokenCounter {
	return &TiktokenTokenCounter{}
}

func (t *TiktokenTokenCounter) init() {
	t.once.Do(func() {
		encoding, err := tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			t.err = err
			return
		}
		t.encoding = encoding
	})
}

// CountTokens returns the exact token count using tiktoken encoding.
// Falls back to the heuristic if tiktoken initialization failed.
func (t *TiktokenTokenCounter) CountTokens(text string) int {
	t.init()
	if t.err != nil || t.encoding == nil {
		// Fallback to heuristic if tiktoken fails
		return len(text) / 4
	}
	return len(t.encoding.Encode(text, nil, nil))
}

// CountMessageTokens returns the exact token count for a message using tiktoken.
func (t *TiktokenTokenCounter) CountMessageTokens(msg *schema.Message) int {
	if msg == nil {
		return 0
	}

	var total int

	// 主内容
	total += t.CountTokens(msg.Content)
	total += t.CountTokens(msg.ReasoningContent)

	// UserInputMultiContent（用户输入多模态内容）
	for _, part := range msg.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			total += t.CountTokens(part.Text)
		}
		// 图片等多模态内容按固定 token 估算（约 250 tokens）
		if part.Type == schema.ChatMessagePartTypeImageURL {
			total += estimatedImageChars / 4
		}
	}

	// AssistantGenMultiContent（模型输出多模态内容）
	for _, part := range msg.AssistantGenMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			total += t.CountTokens(part.Text)
		} else if part.Type == schema.ChatMessagePartTypeReasoning && part.Reasoning != nil {
			total += t.CountTokens(part.Reasoning.Text)
		}
	}

	// ToolCalls（工具调用）
	for _, tc := range msg.ToolCalls {
		total += t.CountTokens(tc.Function.Name)
		total += t.CountTokens(tc.Function.Arguments)
		total += t.CountTokens(tc.ID) // tool call ID
	}

	// Tool 结果消息
	if msg.Role == schema.Tool {
		total += t.CountTokens(msg.ToolCallID)
		total += t.CountTokens(msg.ToolName)
	}

	return total
}

// =============================================================================
// Global TokenCounter
// =============================================================================

// tokenCounterHolder wraps a TokenCounter so atomic.Value always stores the
// same concrete type (*tokenCounterHolder), regardless of which TokenCounter
// implementation is active. Without this wrapper, switching between
// *HeuristicTokenCounter and *TiktokenTokenCounter would panic.
type tokenCounterHolder struct {
	tc TokenCounter
}

var globalTokenCounter atomic.Value // stores *tokenCounterHolder

func init() {
	globalTokenCounter.Store(&tokenCounterHolder{tc: &HeuristicTokenCounter{}})
}

// SetGlobalTokenCounter sets the global token counter instance.
// Call this once at startup based on configuration.
func SetGlobalTokenCounter(tc TokenCounter) {
	if tc != nil {
		globalTokenCounter.Store(&tokenCounterHolder{tc: tc})
	}
}

// GetGlobalTokenCounter returns the global token counter instance.
func GetGlobalTokenCounter() TokenCounter {
	return globalTokenCounter.Load().(*tokenCounterHolder).tc
}

// NewTokenCounter creates a TokenCounter based on the mode.
// mode: "heuristic" (default) or "tokenizer"
func NewTokenCounter(mode string) TokenCounter {
	switch strings.ToLower(mode) {
	case "tokenizer":
		return NewTiktokenTokenCounter()
	default:
		return &HeuristicTokenCounter{}
	}
}

// =============================================================================
// Legacy functions (backward compatibility)
// =============================================================================

// EstimateMessageTokensPrecise estimates token count for a single message
// using the global TokenCounter.
//
// It considers:
//   - Content and ReasoningContent
//   - UserInputMultiContent (user input multimodal content)
//   - AssistantGenMultiContent (model output multimodal content)
//   - ToolCalls (function name, arguments, and ID)
//   - Tool result metadata (ToolCallID, ToolName)
func EstimateMessageTokensPrecise(msg *schema.Message) int {
	return GetGlobalTokenCounter().CountMessageTokens(msg)
}

// CountStringTokens counts the number of tokens in a string using the global TokenCounter.
func CountStringTokens(text string) int {
	return GetGlobalTokenCounter().CountTokens(text)
}

// CumulativeTokenCounter estimates the total token count across all messages.
//
// Strategy:
//   - For messages with ResponseMeta.Usage (from LLM responses), use TotalTokens
//     as the context size baseline at that point in the conversation.
//   - For messages without Usage data, estimate via precise estimation.
//   - Walk backwards to find the last message with Usage, use its TotalTokens
//     as the cumulative baseline, then add estimates for newer messages.
//   - If no messages have Usage data at all, fall back to estimating every
//     message from content length.
func CumulativeTokenCounter(_ context.Context, messages []*schema.Message) (int, error) {
	// 1. 从后向前查找最后一条带 Usage 的消息作为基线（不限角色）。
	var baseTokens int
	incrementStart := 0

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
			usage := msg.ResponseMeta.Usage
			// TotalTokens 已经是准确值（包含 cache read + cache creation）
			if usage.TotalTokens > 0 {
				baseTokens = usage.TotalTokens
				incrementStart = i + 1
				break
			}
		}
	}

	// 2. 累加基线之后新增消息的估算 token（使用精确估算）。
	var estimated int
	for _, msg := range messages[incrementStart:] {
		estimated += EstimateMessageTokensPrecise(msg)
	}

	return baseTokens + estimated, nil
}
