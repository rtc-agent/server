package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// Post-compact file recovery restores recently-read file contents as
// system-reminder attachments after context compression.
//
// When compression discards old messages, any `read` tool results that lived
// in the discarded portion are lost from the LLM's visible context. This
// module scans the discarded messages, extracts the content of the most
// recent `read` calls, and appends them as system-reminder attachments so
// the LLM can resume work without re-reading files it was just working with.
//
// Design notes:
//   - The server has no direct VirtualFS access (files live on the client).
//     Recovery content is sourced from the in-memory messages that are about
//     to be discarded — these already contain the tool results returned by
//     the client, possibly truncated by tool_budget / microcompact.
//   - Attachments are appended in-memory only; they are NOT persisted to the
//     DB. They serve as a one-turn "context refresher" for the LLM call
//     immediately following compression. Subsequent turns can re-read files
//     via the `read` tool if needed.
//   - Only the `read` tool is considered. Other compactable tools (write,
//   grep, find, script) produce outputs that are less useful to restore
//     verbatim.

const (
	// postCompactMaxFiles is the maximum number of file attachments to
	// generate per compression.
	postCompactMaxFiles = 5

	// postCompactMaxTokensPerFile caps the token budget for a single
	// recovered file. Beyond this, content is truncated head+tail.
	postCompactMaxTokensPerFile = 2000

	// postCompactTotalTokenBudget caps the total tokens consumed by all
	// post-compact attachments.
	postCompactTotalTokenBudget = 10000

	// postCompactTruncateMsg is inserted between preserved head and tail
	// when a recovered file's content exceeds the per-file budget.
	postCompactTruncateMsg = "\n\n[Content truncated]\n\n"
)

// postCompactReadArgs is the JSON shape of the `read` tool's arguments.
// The field name must match the registration in tools.go.
type postCompactReadArgs struct {
	Path string `json:"path"`
}

// createPostCompactAttachments scans discarded messages for recent `read`
// tool calls and builds system-reminder attachments containing their
// content.
//
// Algorithm:
//  1. Walk `discarded` forward to collect `read` tool calls with their
//     arguments (path) and tool call IDs.
//  2. Build a map of tool call ID → tool result content from tool-role
//     messages in `discarded`.
//  3. Iterate the collected reads in reverse (most recent first), dedup
//     by path, skip cleared/empty results.
//  4. For each attachment: truncate content if over per-file budget, skip
//     if over total budget, stop after postCompactMaxFiles.
//
// The returned slice contains system-role messages ready to be appended to
// the compressed message list. Returns nil if no attachments are generated.
func (h *helpers) createPostCompactAttachments(
	_ context.Context,
	discarded []*schema.Message,
) []*schema.Message {
	if len(discarded) == 0 {
		return nil
	}

	// 1. Collect read tool calls in order.
	type readCall struct {
		path       string
		toolCallID string
	}
	var reads []readCall
	for _, msg := range discarded {
		if msg.Role != schema.Assistant {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.Function.Name != "read" {
				continue
			}
			var args postCompactReadArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				continue
			}
			if args.Path == "" {
				continue
			}
			reads = append(reads, readCall{
				path:       args.Path,
				toolCallID: tc.ID,
			})
		}
	}
	if len(reads) == 0 {
		return nil
	}

	// 2. Build tool call ID → content map.
	toolResultContent := make(map[string]string, len(discarded))
	for _, msg := range discarded {
		if msg.Role == schema.Tool && msg.ToolCallID != "" {
			toolResultContent[msg.ToolCallID] = msg.Content
		}
	}

	// 3. Iterate from most recent, dedup by path, build attachments.
	seen := make(map[string]struct{})
	var attachments []*schema.Message
	totalTokens := 0

	for i := len(reads) - 1; i >= 0; i-- {
		r := reads[i]
		if _, dup := seen[r.path]; dup {
			continue
		}

		content, ok := toolResultContent[r.toolCallID]
		if !ok || content == "" {
			continue
		}
		// Skip microcompacted / cleared content.
		if content == TIME_BASED_MC_CLEARED_MESSAGE {
			continue
		}

		seen[r.path] = struct{}{}

		tokens := len(content) / 4
		if tokens > postCompactMaxTokensPerFile {
			content = truncateForPostCompact(content, postCompactMaxTokensPerFile)
			tokens = postCompactMaxTokensPerFile
		}
		if totalTokens+tokens > postCompactTotalTokenBudget {
			break
		}
		totalTokens += tokens

		attachments = append(attachments, &schema.Message{
			Role:    schema.System,
			Content: formatPostCompactAttachment(r.path, content),
		})

		if len(attachments) >= postCompactMaxFiles {
			break
		}
	}

	return attachments
}

// truncateForPostCompact applies the head-60% + tail-20% strategy, matching
// tool_budget.go's approach.
func truncateForPostCompact(content string, maxTokens int) string {
	maxChars := maxTokens * 4
	if len(content) <= maxChars {
		return content
	}
	headChars := int(float64(maxChars) * 0.6)
	tailChars := int(float64(maxChars) * 0.2)
	return content[:headChars] + postCompactTruncateMsg + content[len(content)-tailChars:]
}

// formatPostCompactAttachment wraps a recovered file's content in a
// system-reminder block consistent with the existing injection pattern
// (see session_memory_inject.go, user_memory_inject.go).
func formatPostCompactAttachment(path, content string) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	sb.WriteString("Recently read file (content restored after context compression):\n")
	sb.WriteString("Path: ")
	sb.WriteString(path)
	sb.WriteString("\n\n")
	sb.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("</system-reminder>")
	return sb.String()
}

// appendPostCompactAttachments is a convenience used by compressContext to
// append post-compact attachments after the compressed message list.
func appendPostCompactAttachments(
	ctx context.Context,
	h *helpers,
	compressed []*schema.Message,
	discarded []*schema.Message,
) []*schema.Message {
	attachments := h.createPostCompactAttachments(ctx, discarded)
	if len(attachments) == 0 {
		return compressed
	}
	out := make([]*schema.Message, 0, len(compressed)+len(attachments))
	out = append(out, compressed...)
	out = append(out, attachments...)

	if h.enableLLMLogging {
		sessionID := getSessionIDFromContext(ctx)
		h.logIfEnabled(ctx, "postCompact.attachments", map[string]any{
			"session_id":       sessionID.String(),
			"attachment_count": len(attachments),
		})
	}
	return out
}
