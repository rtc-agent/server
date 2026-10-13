package cache_analyzer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Summary represents the summary statistics.
type Summary struct {
	TotalRequests         int
	SessionCount          int
	TotalInputTokens      int
	TotalCacheReadTokens  int
	TotalCacheWriteTokens int
	OverallHitRate        float64
	OverallHitRateStr     string
}

func calculateSummary(requests []*LLMRequest) *Summary {
	total := len(requests)
	if total == 0 {
		return &Summary{}
	}

	sessions := make(map[string]bool)
	var totalInputTokens, totalCacheReadTokens, totalCacheWriteTokens int

	for _, req := range requests {
		if req.CacheStats != nil {
			totalInputTokens += req.CacheStats.InputTokens
			totalCacheReadTokens += req.CacheStats.CacheReadTokens
			totalCacheWriteTokens += req.CacheStats.CacheCreationTokens
		}
		if req.SessionID != "" {
			sessions[req.SessionID] = true
		}
	}

	// Calculate overall hit rate based on total tokens
	var overallHitRate float64
	totalTokens := totalInputTokens + totalCacheReadTokens + totalCacheWriteTokens
	if totalTokens > 0 {
		overallHitRate = float64(totalCacheReadTokens) / float64(totalTokens)
	}

	return &Summary{
		TotalRequests:         total,
		SessionCount:          len(sessions),
		TotalInputTokens:      totalInputTokens,
		TotalCacheReadTokens:  totalCacheReadTokens,
		TotalCacheWriteTokens: totalCacheWriteTokens,
		OverallHitRate:        overallHitRate,
		OverallHitRateStr:     formatPercent(overallHitRate),
	}
}

// DiffData represents the diff between two requests.
type DiffData struct {
	RequestIndex int
	PrevRequest  *LLMRequest
	CurrRequest  *LLMRequest
	MessageDiffs []*MessageDiff
	SystemDiffs  []*SystemDiff
	// Statistics
	AddedCount      int
	RemovedCount    int
	ModifiedCount   int
	AddedRoles      []string
	RemovedRoles    []string
	ModifiedRoles   []string
	AddedIndices    []int
	RemovedIndices  []int
	ModifiedIndices []int
	// System statistics
	SystemAddedCount    int
	SystemRemovedCount  int
	SystemModifiedCount int
}

// SystemDiff represents the diff for a single system block.
type SystemDiff struct {
	PrevBlock *SystemBlock
	CurrBlock *SystemBlock
	Status    string // "added", "removed", "unchanged", "modified"
	Diffs     []DiffChunk
}

// MessageDiff represents the diff for a single message.
type MessageDiff struct {
	PrevMessage *Message
	CurrMessage *Message
	Status      string // "added", "removed", "unchanged", "modified"
	Diffs       []DiffChunk
}

// DiffChunk represents a single diff chunk.
type DiffChunk struct {
	Type string // "equal", "insert", "delete"
	Text string
}

func generateDiffs(requests []*LLMRequest) []*DiffData {
	diffs := make([]*DiffData, 0, len(requests))

	// Create a virtual empty request for the first request to compare against
	emptyRequest := &LLMRequest{
		Messages: []*Message{},
		System:   []*SystemBlock{},
	}

	for i, curr := range requests {
		diffData := &DiffData{
			RequestIndex: i,
			CurrRequest:  curr,
		}

		var prev *LLMRequest
		if i == 0 {
			// First request compares against empty
			prev = emptyRequest
		} else {
			prev = requests[i-1]
		}

		diffData.PrevRequest = prev
		diffData.MessageDiffs = generateMessageDiffs(prev, curr)
		diffData.SystemDiffs = generateSystemDiffs(prev, curr)

		// Calculate message statistics
		for j, msgDiff := range diffData.MessageDiffs {
			switch msgDiff.Status {
			case "added":
				diffData.AddedCount++
				diffData.AddedIndices = append(diffData.AddedIndices, j)
				if msgDiff.CurrMessage != nil {
					diffData.AddedRoles = append(diffData.AddedRoles, msgDiff.CurrMessage.Role)
				}
			case "removed":
				diffData.RemovedCount++
				diffData.RemovedIndices = append(diffData.RemovedIndices, j)
				if msgDiff.PrevMessage != nil {
					diffData.RemovedRoles = append(diffData.RemovedRoles, msgDiff.PrevMessage.Role)
				}
			case "modified":
				diffData.ModifiedCount++
				diffData.ModifiedIndices = append(diffData.ModifiedIndices, j)
				if msgDiff.CurrMessage != nil {
					diffData.ModifiedRoles = append(diffData.ModifiedRoles, msgDiff.CurrMessage.Role)
				}
			}
		}

		// Calculate system statistics
		for _, sysDiff := range diffData.SystemDiffs {
			switch sysDiff.Status {
			case "added":
				diffData.SystemAddedCount++
			case "removed":
				diffData.SystemRemovedCount++
			case "modified":
				diffData.SystemModifiedCount++
			}
		}

		diffs = append(diffs, diffData)
	}

	return diffs
}

func generateMessageDiffs(prev, curr *LLMRequest) []*MessageDiff {
	var messageDiffs []*MessageDiff

	// Compare messages by position (simple approach)
	maxLen := len(prev.Messages)
	if len(curr.Messages) > maxLen {
		maxLen = len(curr.Messages)
	}

	for i := 0; i < maxLen; i++ {
		var prevMsg, currMsg *Message
		if i < len(prev.Messages) {
			prevMsg = prev.Messages[i]
		}
		if i < len(curr.Messages) {
			currMsg = curr.Messages[i]
		}

		if prevMsg == nil && currMsg != nil {
			// Message added
			currContent := formatContentPartsForDiff(currMsg.ContentParts)
			if currContent == "" {
				currContent = currMsg.Content
			}
			messageDiffs = append(messageDiffs, &MessageDiff{
				PrevMessage: nil,
				CurrMessage: currMsg,
				Status:      "added",
				Diffs: []DiffChunk{
					{Type: "insert", Text: currContent},
				},
			})
		} else if prevMsg != nil && currMsg == nil {
			// Message removed
			prevContent := formatContentPartsForDiff(prevMsg.ContentParts)
			if prevContent == "" {
				prevContent = prevMsg.Content
			}
			messageDiffs = append(messageDiffs, &MessageDiff{
				PrevMessage: prevMsg,
				CurrMessage: nil,
				Status:      "removed",
				Diffs: []DiffChunk{
					{Type: "delete", Text: prevContent},
				},
			})
		} else if prevMsg != nil && currMsg != nil {
			if prevMsg.ContentHash == currMsg.ContentHash {
				// Message unchanged
				messageDiffs = append(messageDiffs, &MessageDiff{
					PrevMessage: prevMsg,
					CurrMessage: currMsg,
					Status:      "unchanged",
					Diffs:       nil,
				})
			} else {
				// Message modified - generate line-by-line diff
				prevContent := formatContentPartsForDiff(prevMsg.ContentParts)
				if prevContent == "" {
					prevContent = prevMsg.Content
				}
				currContent := formatContentPartsForDiff(currMsg.ContentParts)
				if currContent == "" {
					currContent = currMsg.Content
				}
				diffs := generateLineDiff(prevContent, currContent)
				messageDiffs = append(messageDiffs, &MessageDiff{
					PrevMessage: prevMsg,
					CurrMessage: currMsg,
					Status:      "modified",
					Diffs:       diffs,
				})
			}
		}
	}

	return messageDiffs
}

func generateSystemDiffs(prev, curr *LLMRequest) []*SystemDiff {
	var systemDiffs []*SystemDiff

	// Compare system blocks by position
	maxLen := len(prev.System)
	if len(curr.System) > maxLen {
		maxLen = len(curr.System)
	}

	for i := 0; i < maxLen; i++ {
		var prevBlock, currBlock *SystemBlock
		if i < len(prev.System) {
			prevBlock = prev.System[i]
		}
		if i < len(curr.System) {
			currBlock = curr.System[i]
		}

		if prevBlock == nil && currBlock != nil {
			// System block added
			systemDiffs = append(systemDiffs, &SystemDiff{
				PrevBlock: nil,
				CurrBlock: currBlock,
				Status:    "added",
				Diffs:     []DiffChunk{{Type: "insert", Text: currBlock.Text}},
			})
		} else if prevBlock != nil && currBlock == nil {
			// System block removed
			systemDiffs = append(systemDiffs, &SystemDiff{
				PrevBlock: prevBlock,
				CurrBlock: nil,
				Status:    "removed",
				Diffs:     []DiffChunk{{Type: "delete", Text: prevBlock.Text}},
			})
		} else if prevBlock != nil && currBlock != nil {
			if prevBlock.ContentHash == currBlock.ContentHash {
				// System block unchanged
				systemDiffs = append(systemDiffs, &SystemDiff{
					PrevBlock: prevBlock,
					CurrBlock: currBlock,
					Status:    "unchanged",
					Diffs:     nil,
				})
			} else {
				// System block modified - generate line-by-line diff
				diffs := generateLineDiff(prevBlock.Text, currBlock.Text)
				systemDiffs = append(systemDiffs, &SystemDiff{
					PrevBlock: prevBlock,
					CurrBlock: currBlock,
					Status:    "modified",
					Diffs:     diffs,
				})
			}
		}
	}

	return systemDiffs
}

// generateLineDiff generates a line-by-line diff between two texts using LCS algorithm.
func generateLineDiff(oldText, newText string) []DiffChunk {
	oldLines := strings.Split(oldText, "\n")
	newLines := strings.Split(newText, "\n")

	// Compute LCS (Longest Common Subsequence) table
	m, n := len(oldLines), len(newLines)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if oldLines[i-1] == newLines[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack to produce diff
	type op struct {
		typ  string // "equal", "delete", "insert"
		text string
	}
	var ops []op
	i, j := m, n
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && oldLines[i-1] == newLines[j-1] {
			ops = append(ops, op{"equal", oldLines[i-1]})
			i--
			j--
		} else if j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]) {
			ops = append(ops, op{"insert", newLines[j-1]})
			j--
		} else {
			ops = append(ops, op{"delete", oldLines[i-1]})
			i--
		}
	}

	// Reverse ops
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}

	// Merge adjacent same-type ops into chunks
	var chunks []DiffChunk
	for _, o := range ops {
		if len(chunks) > 0 && chunks[len(chunks)-1].Type == o.typ {
			chunks[len(chunks)-1].Text += "\n" + o.text
		} else {
			chunks = append(chunks, DiffChunk{Type: o.typ, Text: o.text})
		}
	}

	return chunks
}

// formatContentPartsForDiff formats ContentParts into a readable string for diff comparison.
func formatContentPartsForDiff(parts []ContentPart) string {
	if len(parts) == 0 {
		return ""
	}

	var builder strings.Builder
	for i, part := range parts {
		if i > 0 {
			builder.WriteString("\n")
		}

		switch part.Type {
		case "text":
			builder.WriteString(part.Text)
		case "tool_use":
			fmt.Fprintf(&builder, "[TOOL_USE] %s", part.ToolName)
			if part.ToolID != "" {
				fmt.Fprintf(&builder, " (%s)", part.ToolID)
			}
			if inputJSON, err := json.MarshalIndent(part.ToolInput, "", "  "); err == nil {
				builder.WriteString("\n")
				builder.Write(inputJSON)
			}
		case "tool_result":
			status := "OK"
			if part.IsError {
				status = "ERROR"
			}
			fmt.Fprintf(&builder, "[TOOL_RESULT:%s]", status)
			if len(part.ToolResult) > 0 {
				builder.WriteString("\n")
				builder.WriteString(formatContentPartsForDiff(part.ToolResult))
			}
		}
	}

	return builder.String()
}

// Helper functions for templates
func formatPercent(rate float64) string {
	percent := rate * 100
	return strconv.FormatFloat(percent, 'f', 1, 64) + "%"
}

func hitRateClass(rate float64) string {
	if rate >= 0.8 {
		return "hitrate-high"
	} else if rate >= 0.5 {
		return "hitrate-medium"
	}
	return "hitrate-low"
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// formatChangeSummary formats a summary of message changes.
func formatChangeSummary(added, removed, modified int) string {
	var parts []string
	if added > 0 {
		parts = append(parts, fmt.Sprintf("+%d added", added))
	}
	if removed > 0 {
		parts = append(parts, fmt.Sprintf("-%d removed", removed))
	}
	if modified > 0 {
		parts = append(parts, fmt.Sprintf("~%d modified", modified))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

// formatRoleList formats a list of roles.
func formatRoleList(roles []string) string {
	if len(roles) == 0 {
		return ""
	}
	return strings.Join(roles, ", ")
}

// formatIndexList formats a list of indices as ranges.
func formatIndexList(indices []int) string {
	if len(indices) == 0 {
		return ""
	}
	if len(indices) == 1 {
		return fmt.Sprintf("msg %d", indices[0])
	}

	// Check if consecutive
	isConsecutive := true
	for i := 1; i < len(indices); i++ {
		if indices[i] != indices[i-1]+1 {
			isConsecutive = false
			break
		}
	}

	if isConsecutive {
		return fmt.Sprintf("msg %d-%d", indices[0], indices[len(indices)-1])
	}

	// Format as comma-separated
	parts := make([]string, len(indices))
	for i, idx := range indices {
		parts[i] = fmt.Sprintf("%d", idx)
	}
	return "msg " + strings.Join(parts, ", ")
}

func formatTime(t interface{}) string {
	switch v := t.(type) {
	case time.Time:
		if v.IsZero() {
			return ""
		}
		return v.Format("2006-01-02T15:04:05")
	case string:
		if len(v) >= 19 {
			return v[:19]
		}
		return v
	default:
		return ""
	}
}
