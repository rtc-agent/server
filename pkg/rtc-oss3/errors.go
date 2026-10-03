// Package rtcoss3 implements the rtc-oss3 object storage subsystem.
//
// This package provides:
//   - Storage backend abstraction (Backend interface)
//   - MinIO backend implementation
//   - AWS Signature V4 verification (pure algorithm layer)
//   - Presigned URL generation
//   - Error definitions and XML response helpers
package rtcoss3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"

	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// S3TimeFormat is the canonical time format used in S3 responses.
// S3 uses ISO 8601 with millisecond precision (e.g., "2026-01-02T15:04:05.000Z").
// This is different from RFC3339 (no milliseconds) and http.TimeFormat (RFC1123).
//
// M4: Precision note — S3 spec allows optional milliseconds. The format below
// always outputs 3-digit milliseconds for consistency. Most S3 clients accept
// this format; if strict compatibility is needed with clients that reject
// milliseconds, use time.RFC3339 instead.
const S3TimeFormat = "2006-01-02T15:04:05.000Z"

// Backend sentinel errors.
//
// These errors are returned by Backend implementations (e.g. MinIO) and
// wrapped with %w so that callers can use errors.Is() for detection.
// The handler layer maps them to the corresponding S3Error responses.
var (
	// ErrBackendKeyNotFound indicates the requested key does not exist in the backend.
	ErrBackendKeyNotFound = errors.New("key not found")

	// ErrBackendAccessDenied indicates the backend rejected the request due to
	// insufficient permissions.
	ErrBackendAccessDenied = errors.New("access denied")

	// ErrBackendBucketNotFound indicates the requested bucket does not exist in the
	// backend.
	ErrBackendBucketNotFound = errors.New("bucket not found")

	// ErrBackendInsufficientStorage indicates the backend has insufficient storage
	// space (disk full, quota exceeded, etc.).
	ErrBackendInsufficientStorage = errors.New("insufficient storage")

	// ErrQuotaExceeded indicates the user has exceeded their storage quota.
	// This is a domain-level sentinel; the S3-facing error is ErrRequestQuotaExceeded.
	ErrQuotaExceeded = errors.New("quota exceeded")
)

// S3Error represents an S3-compatible error.
type S3Error struct {
	Code     string
	Message  string
	HTTPCode int
}

// Error implements the error interface.
func (e *S3Error) Error() string { return e.Code + ": " + e.Message }

// xmlErrorResponse is the S3 XML error envelope written to clients.
// Matches the standard S3 Error response format:
//
//	<?xml version="1.0" encoding="UTF-8"?>
//	<Error>
//	  <Code>NoSuchKey</Code>
//	  <Message>The specified key does not exist</Message>
//	  <Resource>/bucket/key</Resource>
//	  <RequestId>...</RequestId>
//	</Error>
type xmlErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
}

// WriteS3Error serializes an S3Error as XML and writes it to w.
// Sets the correct HTTP status code and Content-Type header.
func WriteS3Error(w http.ResponseWriter, s3err *S3Error, resource, requestID string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", requestID)
	w.WriteHeader(s3err.HTTPCode)
	resp := xmlErrorResponse{
		Code:      s3err.Code,
		Message:   s3err.Message,
		Resource:  resource,
		RequestID: requestID,
	}
	// xml.Header is a compile-time constant string; fmt.Fprint cannot fail on it.
	_, _ = fmt.Fprint(w, xml.Header)
	if err := xml.NewEncoder(w).Encode(resp); err != nil {
		// Header already sent; cannot return error response. Log for diagnostics.
		logger.Error(context.Background(), "failed to encode S3 error XML response",
			zap.Error(err))
	}
}

// Standard S3 errors.
var (
	ErrAccessDenied         = &S3Error{"AccessDenied", "Access Denied", http.StatusForbidden}
	ErrBucketNotFound       = &S3Error{"NoSuchBucket", "The specified bucket does not exist", http.StatusNotFound}
	ErrKeyNotFound          = &S3Error{"NoSuchKey", "The specified key does not exist", http.StatusNotFound}
	ErrEntityTooLarge       = &S3Error{"EntityTooLarge", "Your proposed upload exceeds the maximum allowed size", http.StatusRequestEntityTooLarge}
	ErrEntityTooSmall       = &S3Error{"EntityTooSmall", "Your proposed upload is smaller than the minimum allowed size", http.StatusBadRequest}
	ErrRequestQuotaExceeded = &S3Error{"RequestQuotaExceeded", "You have exceeded your storage quota", http.StatusForbidden}
	ErrSlowDown             = &S3Error{"SlowDown", "Please reduce your request rate", http.StatusTooManyRequests}
	ErrMaxUploadsExceeded   = &S3Error{"MaxUploadsExceeded", "You have exceeded the maximum number of concurrent uploads", http.StatusBadRequest}
	ErrInvalidSignature     = &S3Error{"SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided", http.StatusForbidden}
	ErrExpiredToken         = &S3Error{"ExpiredToken", "The provided token has expired", http.StatusForbidden}
	ErrInvalidRequest       = &S3Error{"InvalidRequest", "Invalid request", http.StatusBadRequest}
	ErrInternalError        = &S3Error{"InternalError", "We encountered an internal error", http.StatusInternalServerError}
	ErrUploadNotFound       = &S3Error{"NoSuchUpload", "The specified multipart upload does not exist", http.StatusNotFound}
	ErrMethodNotAllowed     = &S3Error{"MethodNotAllowed", "The specified method is not allowed against this resource", http.StatusMethodNotAllowed}
	ErrPreconditionFailed   = &S3Error{"PreconditionFailed", "At least one of the pre-conditions you specified did not hold", http.StatusPreconditionFailed}
	ErrInvalidArgument      = &S3Error{"InvalidArgument", "Invalid Argument", http.StatusBadRequest}
	ErrMissingHeader        = &S3Error{"MissingSecurityHeader", "Your request is missing a required header", http.StatusBadRequest}
	ErrBadDigest            = &S3Error{"BadDigest", "The Content-MD5 you specified did not match what we received", http.StatusBadRequest}
	ErrIncompleteBody       = &S3Error{"IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header", http.StatusBadRequest}
	ErrLockAcquireFailed    = &S3Error{"LockAcquireFailed", "Failed to acquire lock, please retry", http.StatusConflict}
	ErrInsufficientStorage  = &S3Error{"InsufficientStorage", "Insufficient storage space available", http.StatusInsufficientStorage}                              // 507, MinIO disk full (R10 H3)
	ErrRequestTimeTooSkewed = &S3Error{"RequestTimeTooSkewed", "The difference between the request time and the server's time is too large", http.StatusForbidden} // R10 M3

	// Additional errors for 1E-3 basic operations
	ErrInvalidURI             = &S3Error{"InvalidURI", "The specified URI is invalid", http.StatusBadRequest}
	ErrNotImplemented         = &S3Error{"NotImplemented", "A parameter you provided is not yet implemented", http.StatusNotImplemented}
	ErrMissingContentLength   = &S3Error{"MissingContentLength", "You must provide the Content-Length HTTP header", http.StatusLengthRequired}
	ErrInvalidRange           = &S3Error{"InvalidRange", "The requested range is not satisfiable", http.StatusRequestedRangeNotSatisfiable}
	ErrInvalidCopySource      = &S3Error{"InvalidCopySource", "The specified copy source is not valid", http.StatusBadRequest}
	ErrRateLimitExceeded      = &S3Error{"RateLimitExceeded", "You have exceeded your request rate limit", http.StatusTooManyRequests}
	ErrInvalidKeyFormat       = &S3Error{"InvalidKeyFormat", "The specified key does not match the required format", http.StatusBadRequest}
	ErrUnsupportedContentType = &S3Error{"UnsupportedContentType", "The specified Content-Type is not allowed. Only image/* and text/* types are permitted", http.StatusBadRequest}

	// Additional errors for 1E-4 multipart operations
	ErrInvalidPartNumber = &S3Error{"InvalidPartNumber", "The specified part number is not valid", http.StatusBadRequest}
	ErrInvalidPart       = &S3Error{"InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag", http.StatusBadRequest}
	ErrMalformedXML      = &S3Error{"MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema", http.StatusBadRequest}
)
