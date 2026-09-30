package httphandler

import (
	"encoding/xml"
	"fmt"
	"net/http"

	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// xmlErrorResponse is the S3 XML error envelope.
type xmlErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
}

// WriteS3Error serializes an S3Error as XML and writes it to w.
func WriteS3Error(w http.ResponseWriter, s3err *rtcoss3.S3Error, resource, requestID string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", requestID)
	w.WriteHeader(s3err.HTTPCode)
	resp := xmlErrorResponse{
		Code:      s3err.Code,
		Message:   s3err.Message,
		Resource:  resource,
		RequestID: requestID,
	}
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(resp)
}
