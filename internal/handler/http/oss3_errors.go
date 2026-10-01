package httphandler

// S3 error response types and helpers are provided by pkg/rtc-oss3.
// This file re-exports WriteS3Error for handler-local use, avoiding
// duplication of the xmlErrorResponse struct and XML serialisation logic.

import (
	"net/http"

	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// WriteS3Error serializes an S3Error as XML and writes it to w.
//
// Delegates to rtcoss3.WriteS3Error. Exists in this package so that
// handler code can call WriteS3Error without importing the pkg qualifier.
func WriteS3Error(w http.ResponseWriter, s3err *rtcoss3.S3Error, resource, requestID string) {
	rtcoss3.WriteS3Error(w, s3err, resource, requestID)
}
