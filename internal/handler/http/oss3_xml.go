package httphandler

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"

	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// writeXMLResponse writes a standard S3 XML response: sets Content-Type,
// status code, the xml.Header preamble, and encodes the response struct.
//
// All S3 XML responses follow this pattern. Centralising it avoids the
// 7-call-site duplication that existed in oss3.go and oss3_multipart.go.
//
// Encoding errors are logged because the response headers have already been
// sent at that point; an error response cannot be written.
//
// The statusCode parameter is included for API symmetry even though all
// current S3 XML responses use 200 OK; error responses use rtcoss3.WriteS3Error.
func writeXMLResponse(w http.ResponseWriter, statusCode int, resp interface{}) { //nolint:unparam // statusCode kept for API flexibility
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	_, _ = fmt.Fprint(w, xml.Header) // xml.Header is a compile-time constant; cannot fail
	if err := xml.NewEncoder(w).Encode(resp); err != nil {
		// Header already sent; cannot return error response. Log for diagnostics.
		logger.Error(context.Background(), "failed to encode XML response", zap.Error(err))
	}
}
