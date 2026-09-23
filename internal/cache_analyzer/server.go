package cache_analyzer

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed templates/*.html
var templateFS embed.FS

// Server represents the cache analyzer HTTP server.
type Server struct {
	port     int
	logFiles []string
}

// NewServer creates a new cache analyzer server.
func NewServer(port int, logFiles []string) *Server {
	return &Server{
		port:     port,
		logFiles: logFiles,
	}
}

// Run starts the HTTP server.
func (s *Server) Run() error {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// Load templates with custom functions
	funcMap := template.FuncMap{
		"formatTime":          formatTime,
		"hitRateClass":        hitRateClass,
		"truncate":            truncate,
		"sub":                 func(a, b int) int { return a - b },
		"add":                 func(a, b int) int { return a + b },
		"hitRatePercent":      func(rate float64) string { return formatPercent(rate) },
		"formatChangeSummary": formatChangeSummary,
		"formatRoleList":      formatRoleList,
		"formatIndexList":     formatIndexList,
	}
	r.SetFuncMap(funcMap)

	// Load templates from embedded filesystem
	templates, err := fs.Sub(templateFS, "templates")
	if err != nil {
		return err
	}
	r.LoadHTMLGlob("templates/*.html")
	// Parse templates from embedded FS
	tmpl := template.Must(template.New("").Funcs(funcMap).ParseFS(templates, "*.html"))
	r.SetHTMLTemplate(tmpl)

	// Routes
	r.GET("/", s.handleConfigPage)
	r.POST("/report", s.handleReportPage)
	r.GET("/export/:session_id", s.handleExportMarkdown)

	addr := ":" + strconv.Itoa(s.port)
	return r.Run(addr)
}

func (s *Server) handleConfigPage(c *gin.Context) {
	c.HTML(http.StatusOK, "config.html", gin.H{
		"Title": "Cache Analyzer Configuration",
	})
}

func (s *Server) handleReportPage(c *gin.Context) {
	sessionID := c.PostForm("session_id")
	if sessionID == "" {
		c.String(http.StatusBadRequest, "Session ID is required")
		return
	}

	// Get log files from form (can override server config)
	logFiles := c.PostFormArray("log_paths[]")
	if len(logFiles) == 0 {
		logFiles = s.logFiles
	}

	if len(logFiles) == 0 {
		c.String(http.StatusBadRequest, "No log files specified")
		return
	}

	// Parse and filter requests
	requests, err := MergeAndFilterRequests(logFiles, sessionID)
	if err != nil {
		c.String(http.StatusInternalServerError, "Error parsing logs: %v", err)
		return
	}

	if len(requests) == 0 {
		c.String(http.StatusNotFound, "No requests found for session: %s", sessionID)
		return
	}

	// Calculate summary statistics
	summary := calculateSummary(requests)

	// Generate diff data for each request
	diffs := generateDiffs(requests)

	c.HTML(http.StatusOK, "report.html", gin.H{
		"Title":     "Cache Analysis Report",
		"SessionID": sessionID,
		"Summary":   summary,
		"Requests":  requests,
		"Diffs":     diffs,
		"LogFiles":  strings.Join(logFiles, ", "),
	})
}

// Summary represents the summary statistics.
type Summary struct {
	TotalRequests int
	AvgHitRate    float64
	AvgHitRateStr string
	SessionCount  int
}

func calculateSummary(requests []*LLMRequest) *Summary {
	total := len(requests)
	if total == 0 {
		return &Summary{}
	}

	var totalHitRate float64
	sessions := make(map[string]bool)

	for _, req := range requests {
		if req.CacheStats != nil {
			totalHitRate += req.CacheStats.HitRate()
		}
		if req.SessionID != "" {
			sessions[req.SessionID] = true
		}
	}

	avgHitRate := totalHitRate / float64(total)

	return &Summary{
		TotalRequests: total,
		AvgHitRate:    avgHitRate,
		AvgHitRateStr: formatPercent(avgHitRate),
		SessionCount:  len(sessions),
	}
}

// DiffData represents the diff between two requests.
type DiffData struct {
	RequestIndex int
	PrevRequest  *LLMRequest
	CurrRequest  *LLMRequest
	MessageDiffs []*MessageDiff
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

	for i, curr := range requests {
		diffData := &DiffData{
			RequestIndex: i,
			CurrRequest:  curr,
		}

		if i > 0 {
			prev := requests[i-1]
			diffData.PrevRequest = prev
			diffData.MessageDiffs = generateMessageDiffs(prev, curr)

			// Calculate statistics
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

func (s *Server) handleExportMarkdown(c *gin.Context) {
	sessionID := c.Param("session_id")
	if sessionID == "" {
		c.String(http.StatusBadRequest, "Session ID is required")
		return
	}

	// Parse log paths from query parameters
	logFiles := c.QueryArray("log_paths[]")
	if len(logFiles) == 0 {
		logFiles = s.logFiles
	}

	if len(logFiles) == 0 {
		c.String(http.StatusBadRequest, "No log files specified. Please add log paths in the config page or use --log parameter")
		return
	}

	// Parse from and to parameters
	fromStr := c.DefaultQuery("from", "")
	toStr := c.DefaultQuery("to", "")

	fromIndex, toIndex := -1, -1
	if fromStr != "" && toStr != "" {
		from, err1 := strconv.Atoi(fromStr)
		to, err2 := strconv.Atoi(toStr)
		if err1 == nil && err2 == nil {
			fromIndex = from
			toIndex = to
		}
	}

	// Parse and filter requests
	requests, err := MergeAndFilterRequests(logFiles, sessionID)
	if err != nil {
		c.String(http.StatusInternalServerError, "Error parsing logs: %v", err)
		return
	}

	if len(requests) == 0 {
		c.String(http.StatusNotFound, "No requests found for session: %s", sessionID)
		return
	}

	// Generate markdown content
	var markdown string
	if fromIndex >= 0 && toIndex >= 0 && fromIndex < len(requests) && toIndex < len(requests) {
		// Export specific diff
		markdown = generateSingleDiffMarkdown(sessionID, logFiles, requests, fromIndex, toIndex)
	} else {
		// Export all diffs
		markdown = generateMarkdownReport(sessionID, requests)
	}

	// Set headers for file download
	var filename string
	if fromIndex >= 0 && toIndex >= 0 {
		filename = fmt.Sprintf("cache-diff-%s-%d-vs-%d.md", sessionID[:8], fromIndex, toIndex)
	} else {
		filename = fmt.Sprintf("cache-analysis-%s.md", sessionID[:8])
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Data(http.StatusOK, "text/markdown; charset=utf-8", []byte(markdown))
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

	// Calculate average hit rate
	var totalHitRate float64
	count := 0
	for _, req := range requests {
		if req.CacheStats != nil {
			totalHitRate += req.CacheStats.HitRate()
			count++
		}
	}
	if count > 0 {
		avgHitRate := totalHitRate / float64(count)
		fmt.Fprintf(&sb, "**Average Hit Rate**: %s  \n", formatPercent(avgHitRate))
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
			sb.WriteString("\n\n")
		}

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
	sb.WriteString("\n\n")

	// Generate diff
	diffs := generateDiffs(requests)
	if toIndex < len(diffs) {
		diff := diffs[toIndex]
		writeMessageDiffs(&sb, diff.MessageDiffs)
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
