package agent

import (
	_ "embed"
	"strings"
)

// Embed prompt files for context compression.
//
// These prompts implement the 9-part structured summarization format
// inspired by Claude Code's context management design. The prompts
// instruct the LLM to produce <analysis> and <summary> blocks.
//
//go:embed prompts/no-tools-preamble.md
var noToolsPreamble string

//go:embed prompts/base-compact-prompt.md
var baseCompactPrompt string

//go:embed prompts/partial-compact-prompt.md
var partialCompactPrompt string

//go:embed prompts/partial-compact-up-to-prompt.md
var partialCompactUpToPrompt string

//go:embed prompts/no-tools-trailer.md
var noToolsTrailer string

//go:embed prompts/compact-user-summary-message.md
var compactUserSummaryMessage string

// CompactMode determines which prompt template to use for summarization.
type CompactMode int

const (
	// CompactModeFull compresses the entire conversation history.
	// Use when there is no retained context (all messages will be summarized).
	CompactModeFull CompactMode = iota

	// CompactModePartial compresses only the older messages, keeping
	// recent messages intact. The summary focuses on the older portion.
	CompactModePartial

	// CompactModePartialUpTo compresses messages up to a certain point,
	// for use when newer messages will follow the summary in a continuing
	// session.
	CompactModePartialUpTo
)

// getCompactPrompt returns the full compression prompt for the given mode.
//
// The prompt is assembled from three parts:
//  1. no-tools-preamble: Instructs the LLM to respond with text only.
//  2. base/partial prompt: The core summarization instructions.
//  3. no-tools-trailer: Reminder to not call tools.
func getCompactPrompt(mode CompactMode) string {
	var sb strings.Builder

	sb.WriteString(strings.TrimSpace(noToolsPreamble))
	sb.WriteString("\n\n")

	switch mode {
	case CompactModeFull:
		sb.WriteString(strings.TrimSpace(baseCompactPrompt))
	case CompactModePartial:
		sb.WriteString(strings.TrimSpace(partialCompactPrompt))
	case CompactModePartialUpTo:
		sb.WriteString(strings.TrimSpace(partialCompactUpToPrompt))
	}

	sb.WriteString("\n\n")
	sb.WriteString(strings.TrimSpace(noToolsTrailer))

	return sb.String()
}

// formatCompactUserMessage formats the user message that wraps the summary
// after compaction. It replaces placeholders in the template with actual values.
//
// Placeholders:
//   - {formattedSummary}: The LLM-generated summary text.
func formatCompactUserMessage(formattedSummary string) string {
	msg := strings.TrimSpace(compactUserSummaryMessage)
	msg = strings.ReplaceAll(msg, "{formattedSummary}", formattedSummary)
	// Note: {transcriptPath} is not used in RTC-Agent currently because
	// the frontend provides access to historical messages directly.
	msg = strings.ReplaceAll(msg, "{transcriptPath}", "the session history")
	return msg
}

// formatCompactSummary processes the raw LLM output to extract the summary.
//
// The LLM is instructed to produce <analysis>...</analysis> followed by
// <summary>...</summary>. This function:
//  1. Strips the <analysis> block entirely.
//  2. Extracts the content within <summary> tags.
//  3. Returns the cleaned summary text.
//
// If the expected tags are not found, the raw text is returned as-is
// (fallback for unexpected LLM behavior).
func formatCompactSummary(rawOutput string) string {
	rawOutput = strings.TrimSpace(rawOutput)

	// Strip <analysis>...</analysis> block (if present).
	if idx := strings.Index(rawOutput, "<analysis>"); idx >= 0 {
		if endIdx := strings.Index(rawOutput, "</analysis>"); endIdx > idx {
			// Remove everything from <analysis> to </analysis> inclusive.
			rawOutput = rawOutput[:idx] + rawOutput[endIdx+len("</analysis>"):]
			rawOutput = strings.TrimSpace(rawOutput)
		}
	}

	// Extract <summary>...</summary> content (if present).
	if startIdx := strings.Index(rawOutput, "<summary>"); startIdx >= 0 {
		if endIdx := strings.Index(rawOutput, "</summary>"); endIdx > startIdx {
			summaryContent := rawOutput[startIdx+len("<summary>") : endIdx]
			return strings.TrimSpace(summaryContent)
		}
	}

	// Fallback: return as-is if tags are not found.
	return rawOutput
}
