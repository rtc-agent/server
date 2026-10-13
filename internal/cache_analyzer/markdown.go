package cache_analyzer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// writeSystemDiffs writes system diffs to the string builder in markdown format.
func writeSystemDiffs(sb *strings.Builder, systemDiffs []*SystemDiff) {
	if len(systemDiffs) == 0 {
		return
	}

	sb.WriteString("### System Prompt Changes\n\n")

	for j, sysDiff := range systemDiffs {
		if sysDiff.Status == "unchanged" {
			continue
		}

		statusText := ""
		switch sysDiff.Status {
		case "added":
			statusText = "Added"
		case "removed":
			statusText = "Removed"
		case "modified":
			statusText = "Modified"
		}

		fmt.Fprintf(sb, "#### System Block %d - %s\n\n", j+1, statusText)

		// Show full content first
		if sysDiff.PrevBlock != nil {
			sb.WriteString("**Previous content:**\n")
			sb.WriteString("````\n")
			sb.WriteString(sysDiff.PrevBlock.Text)
			sb.WriteString("\n````\n\n")
		}
		if sysDiff.CurrBlock != nil {
			sb.WriteString("**Current content:**\n")
			sb.WriteString("````\n")
			sb.WriteString(sysDiff.CurrBlock.Text)
			sb.WriteString("\n````\n\n")
		}

		// Then show diff
		sb.WriteString("**Diff:**\n")
		sb.WriteString("````diff\n")

		// Write diff lines
		for _, chunk := range sysDiff.Diffs {
			var prefix string
			switch chunk.Type {
			case "equal":
				prefix = " "
			case "insert":
				prefix = "+"
			case "delete":
				prefix = "-"
			}

			// Split by lines and add prefix to each line
			lines := strings.Split(chunk.Text, "\n")
			for _, line := range lines {
				if line != "" || chunk.Type != "equal" {
					sb.WriteString(prefix + line + "\n")
				}
			}
		}

		sb.WriteString("````\n\n")
	}
}

// writeMessageDiffs writes message diffs to the string builder in markdown format.
func writeMessageDiffs(sb *strings.Builder, messageDiffs []*MessageDiff) {
	if len(messageDiffs) == 0 {
		return
	}

	sb.WriteString("### Message Changes\n\n")

	for j, msgDiff := range messageDiffs {
		if msgDiff.Status == "unchanged" {
			continue
		}

		// Determine role and status
		var role string
		if msgDiff.CurrMessage != nil {
			role = msgDiff.CurrMessage.Role
		} else if msgDiff.PrevMessage != nil {
			role = msgDiff.PrevMessage.Role
		}

		statusText := ""
		switch msgDiff.Status {
		case "added":
			statusText = "Added"
		case "removed":
			statusText = "Removed"
		case "modified":
			statusText = "Modified"
		}

		fmt.Fprintf(sb, "#### Message %d (%s) - %s\n\n", j+1, role, statusText)

		// Show full content first (using ContentParts for complete info)
		if msgDiff.PrevMessage != nil {
			sb.WriteString("**Previous content:**\n")
			sb.WriteString("````\n")
			prevContent := formatContentPartsForDiff(msgDiff.PrevMessage.ContentParts)
			if prevContent == "" {
				prevContent = msgDiff.PrevMessage.Content
			}
			sb.WriteString(prevContent)
			sb.WriteString("\n````\n\n")
		}
		if msgDiff.CurrMessage != nil {
			sb.WriteString("**Current content:**\n")
			sb.WriteString("````\n")
			currContent := formatContentPartsForDiff(msgDiff.CurrMessage.ContentParts)
			if currContent == "" {
				currContent = msgDiff.CurrMessage.Content
			}
			sb.WriteString(currContent)
			sb.WriteString("\n````\n\n")
		}

		// Then show diff
		sb.WriteString("**Diff:**\n")
		sb.WriteString("````diff\n")

		// Write diff lines
		for _, chunk := range msgDiff.Diffs {
			var prefix string
			switch chunk.Type {
			case "equal":
				prefix = " "
			case "insert":
				prefix = "+"
			case "delete":
				prefix = "-"
			}

			// Split by lines and add prefix to each line
			lines := strings.Split(chunk.Text, "\n")
			for _, line := range lines {
				if line != "" || chunk.Type != "equal" {
					sb.WriteString(prefix + line + "\n")
				}
			}
		}

		sb.WriteString("````\n\n")
	}
}

// generateMarkdownReport generates a markdown report for the given session.
func generateMarkdownReport(sessionID string, requests []*LLMRequest) string {
	var sb strings.Builder

	// Header
	sb.WriteString("# Cache Analysis Report\n\n")
	fmt.Fprintf(&sb, "**Session ID**: %s  \n", sessionID)
	fmt.Fprintf(&sb, "**Generated**: %s  \n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&sb, "**Total Requests**: %d  \n", len(requests))

	// Calculate overall hit rate based on total tokens
	summary := calculateSummary(requests)
	if summary.TotalInputTokens > 0 || summary.TotalCacheReadTokens > 0 || summary.TotalCacheWriteTokens > 0 {
		fmt.Fprintf(&sb, "**Overall Cache Hit Rate**: %s  \n", summary.OverallHitRateStr)
		sb.WriteString("\n**Token Usage:**\n")
		fmt.Fprintf(&sb, "- Total Input: %d  \n", summary.TotalInputTokens)
		fmt.Fprintf(&sb, "- Cache Write: %d  \n", summary.TotalCacheWriteTokens)
		fmt.Fprintf(&sb, "- Cache Read: %d  \n", summary.TotalCacheReadTokens)
		fmt.Fprintf(&sb, "- Total: %d  \n", summary.TotalInputTokens+summary.TotalCacheWriteTokens+summary.TotalCacheReadTokens)
	}
	sb.WriteString("\n---\n\n")

	// Generate diffs
	diffs := generateDiffs(requests)

	for i, diff := range diffs {
		if i == 0 {
			continue // Skip first request (no previous to compare)
		}

		fmt.Fprintf(&sb, "## Request %d vs Request %d\n\n", i-1, i)

		// Metadata
		if diff.PrevRequest != nil && diff.CurrRequest != nil {
			prevTime := diff.PrevRequest.Timestamp.Format("2006-01-02T15:04:05")
			currTime := diff.CurrRequest.Timestamp.Format("2006-01-02T15:04:05")
			fmt.Fprintf(&sb, "**Time**: %s → %s  \n", prevTime, currTime)

			if diff.PrevRequest.CacheStats != nil && diff.CurrRequest.CacheStats != nil {
				fmt.Fprintf(&sb, "**Cache Hit Rate**: %s → %s  \n",
					diff.PrevRequest.CacheStats.HitRatePercent(),
					diff.CurrRequest.CacheStats.HitRatePercent())
			}

			prevCount := diff.PrevRequest.MessageCount()
			currCount := diff.CurrRequest.MessageCount()
			fmt.Fprintf(&sb, "**Messages**: %d → %d", prevCount, currCount)

			if currCount > prevCount {
				fmt.Fprintf(&sb, " (+%d added)", currCount-prevCount)
			} else if currCount < prevCount {
				fmt.Fprintf(&sb, " (-%d removed)", prevCount-currCount)
			}
			sb.WriteString("  \n")

			// System changes
			prevSysCount := len(diff.PrevRequest.System)
			currSysCount := len(diff.CurrRequest.System)
			fmt.Fprintf(&sb, "**System**: %d → %d", prevSysCount, currSysCount)
			if diff.SystemAddedCount > 0 || diff.SystemRemovedCount > 0 || diff.SystemModifiedCount > 0 {
				fmt.Fprintf(&sb, " (%s)", formatChangeSummary(diff.SystemAddedCount, diff.SystemRemovedCount, diff.SystemModifiedCount))
			}
			sb.WriteString("\n\n")
		}

		// System diffs
		writeSystemDiffs(&sb, diff.SystemDiffs)

		// Message diffs
		writeMessageDiffs(&sb, diff.MessageDiffs)

		sb.WriteString("---\n\n")
	}

	return sb.String()
}

// generateSingleDiffMarkdown generates a markdown report for a single diff between two requests.
func generateSingleDiffMarkdown(sessionID string, logFiles []string, requests []*LLMRequest, fromIndex, toIndex int) string {
	var sb strings.Builder

	if fromIndex >= len(requests) || toIndex >= len(requests) {
		return "# Error: Invalid request indices\n"
	}

	prevReq := requests[fromIndex]
	currReq := requests[toIndex]

	// Header
	sb.WriteString("# Cache Diff Report\n\n")
	fmt.Fprintf(&sb, "**Session ID**: %s  \n", sessionID)
	fmt.Fprintf(&sb, "**Generated**: %s  \n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&sb, "**Comparing**: Request %d vs Request %d  \n", fromIndex, toIndex)
	sb.WriteString("\n---\n\n")

	// Metadata
	prevTime := prevReq.Timestamp.Format("2006-01-02T15:04:05")
	currTime := currReq.Timestamp.Format("2006-01-02T15:04:05")
	fmt.Fprintf(&sb, "## Request %d vs Request %d\n\n", fromIndex, toIndex)
	fmt.Fprintf(&sb, "**Time**: %s → %s  \n", prevTime, currTime)

	if prevReq.CacheStats != nil && currReq.CacheStats != nil {
		fmt.Fprintf(&sb, "**Cache Hit Rate**: %s → %s  \n",
			prevReq.CacheStats.HitRatePercent(),
			currReq.CacheStats.HitRatePercent())
	}

	prevCount := prevReq.MessageCount()
	currCount := currReq.MessageCount()
	fmt.Fprintf(&sb, "**Messages**: %d → %d", prevCount, currCount)

	if currCount > prevCount {
		fmt.Fprintf(&sb, " (+%d added)", currCount-prevCount)
	} else if currCount < prevCount {
		fmt.Fprintf(&sb, " (-%d removed)", prevCount-currCount)
	}
	sb.WriteString("  \n")

	// System changes
	prevSysCount := len(prevReq.System)
	currSysCount := len(currReq.System)
	fmt.Fprintf(&sb, "**System**: %d → %d", prevSysCount, currSysCount)

	// Generate diff to get system statistics
	diffs := generateDiffs(requests)
	if toIndex < len(diffs) {
		diff := diffs[toIndex]
		if diff.SystemAddedCount > 0 || diff.SystemRemovedCount > 0 || diff.SystemModifiedCount > 0 {
			fmt.Fprintf(&sb, " (%s)", formatChangeSummary(diff.SystemAddedCount, diff.SystemRemovedCount, diff.SystemModifiedCount))
		}
		sb.WriteString("\n\n")

		// System diffs
		writeSystemDiffs(&sb, diff.SystemDiffs)

		// Message diffs
		writeMessageDiffs(&sb, diff.MessageDiffs)
	} else {
		sb.WriteString("\n\n")
	}

	// Add full request bodies at the end
	sb.WriteString("---\n\n")
	sb.WriteString("## Full Request Bodies\n\n")
	sb.WriteString("> **Note**: `tools` 字段完全相同，所以已被省略。\n\n")

	// Request from
	fmt.Fprintf(&sb, "### Request %d\n\n", fromIndex)
	sb.WriteString("````json\n")
	fullReqFrom := getFullRequestBody(logFiles, sessionID, fromIndex)
	sb.WriteString(fullReqFrom)
	sb.WriteString("\n````\n\n")

	// Request to
	fmt.Fprintf(&sb, "### Request %d\n\n", toIndex)
	sb.WriteString("````json\n")
	fullReqTo := getFullRequestBody(logFiles, sessionID, toIndex)
	sb.WriteString(fullReqTo)
	sb.WriteString("\n````\n\n")

	return sb.String()
}

// getFullRequestBody extracts the full request body from log files for a specific request index.
func getFullRequestBody(logFiles []string, sessionID string, requestIndex int) string {
	totalFound := 0
	currentIndex := 0 // Move outside the file loop to maintain continuity across files

	// Re-parse to get the raw request body
	for _, logFile := range logFiles {
		file, err := os.Open(logFile)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(file)
		buf := make([]byte, 0, 1024*1024)
		scanner.Buffer(buf, 10*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var entry map[string]interface{}
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}

			event, _ := entry["msg"].(string)
			sid, _ := entry["session_id"].(string)

			if event == "llm.http.request" && sid == sessionID {
				totalFound++
				if currentIndex == requestIndex {
					requestBody, _ := entry["request_body"].(string)
					// Parse JSON to remove tools field and add debug info
					var reqMap map[string]interface{}
					if err := json.Unmarshal([]byte(requestBody), &reqMap); err != nil {
						// If parsing fails, return original
						var prettyJSON bytes.Buffer
						if err := json.Indent(&prettyJSON, []byte(requestBody), "", "  "); err != nil {
							return requestBody
						}
						return prettyJSON.String()
					}

					// Add debug info about message count
					if msgs, ok := reqMap["messages"].([]interface{}); ok {
						// Prepend debug comment
						debugInfo := fmt.Sprintf("// Debug: Found at index %d, messages in JSON: %d\n", requestIndex, len(msgs))

						// Remove tools field to save space
						delete(reqMap, "tools")

						// Pretty print JSON
						var prettyJSON bytes.Buffer
						if err := json.Indent(&prettyJSON, marshalJSON(reqMap), "", "  "); err != nil {
							return requestBody
						}
						return debugInfo + prettyJSON.String()
					}

					// Remove tools field to save space
					delete(reqMap, "tools")

					// Pretty print JSON
					var prettyJSON bytes.Buffer
					if err := json.Indent(&prettyJSON, marshalJSON(reqMap), "", "  "); err != nil {
						return requestBody
					}
					return prettyJSON.String()
				}
				currentIndex++
			}
		}
		_ = file.Close()
	}

	return fmt.Sprintf("// Error: Request not found (looking for index %d, found %d requests for session %s)", requestIndex, totalFound, sessionID)
}

// marshalJSON marshals a map to JSON bytes.
func marshalJSON(m map[string]interface{}) []byte {
	b, _ := json.Marshal(m)
	return b
}
