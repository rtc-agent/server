package cache_analyzer

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed templates/*.html
var templateFS embed.FS

// Prometheus metrics for cache analyzer
var (
	analysisRequestsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cache_analyzer_analysis_requests_total",
		Help: "Total number of analysis requests",
	})
	analysisDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "cache_analyzer_analysis_duration_seconds",
		Help:    "Duration of analysis requests",
		Buckets: prometheus.DefBuckets,
	})
	overallHitRate = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cache_analyzer_overall_hit_rate",
		Help: "Overall cache hit rate from the most recent analysis",
	})
	latestInputTokens = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cache_analyzer_latest_input_tokens",
		Help: "Input tokens from the most recent analysis (set, not accumulated)",
	})
	latestCacheReadTokens = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cache_analyzer_latest_cache_read_tokens",
		Help: "Cache read tokens from the most recent analysis (set, not accumulated)",
	})
)

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
	tmpl := template.Must(template.New("").Funcs(funcMap).ParseFS(templates, "*.html"))
	r.SetHTMLTemplate(tmpl)

	// Routes
	r.GET("/", s.handleConfigPage)
	r.POST("/report", s.handleReportPage)
	r.GET("/export/:session_id", s.handleExportMarkdown)
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	addr := "127.0.0.1:" + strconv.Itoa(s.port)
	return r.Run(addr)
}

func (s *Server) handleConfigPage(c *gin.Context) {
	c.HTML(http.StatusOK, "config.html", gin.H{
		"Title": "Cache Analyzer Configuration",
	})
}

func (s *Server) handleReportPage(c *gin.Context) {
	start := time.Now()
	defer func() {
		analysisDuration.Observe(time.Since(start).Seconds())
	}()

	sessionID := c.PostForm("session_id")
	if sessionID == "" {
		c.String(http.StatusBadRequest, "Session ID is required")
		return
	}

	// Increment counter only after basic validation passes
	analysisRequestsTotal.Inc()

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

	// Record Prometheus metrics
	overallHitRate.Set(summary.OverallHitRate)
	latestInputTokens.Set(float64(summary.TotalInputTokens))
	latestCacheReadTokens.Set(float64(summary.TotalCacheReadTokens))

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
