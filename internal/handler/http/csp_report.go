package httphandler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// CSPReport represents the structure of a CSP violation report sent by browsers.
// See https://developer.mozilla.org/en-US/docs/Web/HTTP/CSP#violation_report_syntax
type CSPReport struct {
	CSPReport CSPReportBody `json:"csp-report"`
}

// CSPReportBody contains the actual violation details.
type CSPReportBody struct {
	// DocumentURI is the URL of the document where the violation occurred.
	DocumentURI string `json:"document-uri"`
	// Referrer is the referrer of the document.
	Referrer string `json:"referrer"`
	// ViolatedDirective is the directive that was violated.
	ViolatedDirective string `json:"violated-directive"`
	// EffectiveDirective is the directive that actually enforced the policy.
	EffectiveDirective string `json:"effective-directive"`
	// OriginalPolicy is the original CSP header value.
	OriginalPolicy string `json:"original-policy"`
	// BlockedURI is the URI that was blocked.
	BlockedURI string `json:"blocked-uri"`
	// StatusCode is the HTTP status code of the page where violation occurred.
	StatusCode int `json:"status-code"`
}

// HandleCSPReport handles CSP violation reports from browsers.
// Browsers POST JSON reports to the configured report-uri endpoint.
// This handler logs the violation details for monitoring and debugging.
func HandleCSPReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var report CSPReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		http.Error(w, "Invalid report format", http.StatusBadRequest)
		return
	}

	// Log the CSP violation with relevant details
	logger.Warn(r.Context(), "CSP violation",
		zap.String("document_uri", report.CSPReport.DocumentURI),
		zap.String("violated_directive", report.CSPReport.ViolatedDirective),
		zap.String("effective_directive", report.CSPReport.EffectiveDirective),
		zap.String("blocked_uri", report.CSPReport.BlockedURI),
		zap.String("original_policy", report.CSPReport.OriginalPolicy),
		zap.Int("status_code", report.CSPReport.StatusCode),
	)

	w.WriteHeader(http.StatusOK)
}

// HandleGinCSPReport handles CSP violation reports for Gin router.
// This is a wrapper that adapts the standard HTTP handler to Gin's context.
func HandleGinCSPReport(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "Method not allowed"})
		return
	}

	var report CSPReport
	if err := c.ShouldBindJSON(&report); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid report format"})
		return
	}

	// Log the CSP violation with relevant details
	logger.Warn(c.Request.Context(), "CSP violation",
		zap.String("document_uri", report.CSPReport.DocumentURI),
		zap.String("violated_directive", report.CSPReport.ViolatedDirective),
		zap.String("effective_directive", report.CSPReport.EffectiveDirective),
		zap.String("blocked_uri", report.CSPReport.BlockedURI),
		zap.String("original_policy", report.CSPReport.OriginalPolicy),
		zap.Int("status_code", report.CSPReport.StatusCode),
	)

	c.Status(http.StatusOK)
}
