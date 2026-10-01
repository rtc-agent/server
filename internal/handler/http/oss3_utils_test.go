package httphandler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

func TestParseS3Path(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantBucket string
		wantKey    string
		wantOK     bool
	}{
		{
			name:       "valid bucket and key",
			path:       "/mybucket/path/to/file.txt",
			wantBucket: "mybucket",
			wantKey:    "path/to/file.txt",
			wantOK:     true,
		},
		{
			name:       "valid bucket only",
			path:       "/mybucket",
			wantBucket: "mybucket",
			wantKey:    "",
			wantOK:     true,
		},
		{
			name:       "bucket with trailing slash",
			path:       "/mybucket/",
			wantBucket: "mybucket",
			wantKey:    "",
			wantOK:     true,
		},
		{
			name:       "empty path",
			path:       "/",
			wantBucket: "",
			wantKey:    "",
			wantOK:     false,
		},
		{
			name:       "completely empty path",
			path:       "",
			wantBucket: "",
			wantKey:    "",
			wantOK:     false,
		},
		{
			name:       "key with special characters",
			path:       "/mybucket/path with spaces/file%20name.txt",
			wantBucket: "mybucket",
			wantKey:    "path with spaces/file%20name.txt",
			wantOK:     true,
		},
		{
			name:       "path traversal - parent directory",
			path:       "/mybucket/../other/file.txt",
			wantBucket: "",
			wantKey:    "",
			wantOK:     false,
		},
		{
			name:       "path traversal - bare ..",
			path:       "/mybucket/..",
			wantBucket: "",
			wantKey:    "",
			wantOK:     false,
		},
		{
			name:       "path traversal - leading ..",
			path:       "/../mybucket/file.txt",
			wantBucket: "",
			wantKey:    "",
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBucket, gotKey, gotOK := parseS3Path(tt.path)
			if gotOK != tt.wantOK {
				t.Errorf("parseS3Path() ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotBucket != tt.wantBucket {
				t.Errorf("parseS3Path() bucket = %v, want %v", gotBucket, tt.wantBucket)
			}
			if gotKey != tt.wantKey {
				t.Errorf("parseS3Path() key = %v, want %v", gotKey, tt.wantKey)
			}
		})
	}
}

func TestParseRangeHeader(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		wantStart int64
		wantEnd   int64
		wantOK    bool
	}{
		{
			name:      "valid range",
			header:    "bytes=0-499",
			wantStart: 0,
			wantEnd:   499,
			wantOK:    true,
		},
		{
			name:      "valid range with larger numbers",
			header:    "bytes=1000-2000",
			wantStart: 1000,
			wantEnd:   2000,
			wantOK:    true,
		},
		{
			name:      "open-ended range",
			header:    "bytes=500-",
			wantStart: 500,
			wantEnd:   -1,
			wantOK:    true,
		},
		{
			name:      "suffix range",
			header:    "bytes=-500",
			wantStart: -500,
			wantEnd:   -1,
			wantOK:    true,
		},
		{
			name:   "empty header",
			header: "",
			wantOK: false,
		},
		{
			name:   "invalid format - no bytes prefix",
			header: "0-499",
			wantOK: false,
		},
		{
			name:   "invalid format - missing equals",
			header: "bytes0-499",
			wantOK: false,
		},
		{
			name:   "invalid format - single number",
			header: "bytes=500",
			wantOK: false,
		},
		{
			name:   "invalid format - multiple dashes",
			header: "bytes=0-499-100",
			wantOK: false,
		},
		{
			name:   "invalid format - non-numeric",
			header: "bytes=abc-def",
			wantOK: false,
		},
		{
			name:   "invalid suffix range - zero length",
			header: "bytes=-0",
			wantOK: false,
		},
		{
			name:   "invalid suffix range - empty suffix",
			header: "bytes=-",
			wantOK: false,
		},
		{
			name:   "invalid range - end before start",
			header: "bytes=500-100",
			wantOK: false,
		},
		{
			name:   "invalid range - negative start",
			header: "bytes=-1-100",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStart, gotEnd, gotHasRange, gotErr := parseRangeHeader(tt.header)
			if gotHasRange != tt.wantOK {
				t.Errorf("parseRangeHeader() hasRange = %v, want %v", gotHasRange, tt.wantOK)
			}
			if gotHasRange && gotErr == nil {
				if gotStart != tt.wantStart {
					t.Errorf("parseRangeHeader() start = %v, want %v", gotStart, tt.wantStart)
				}
				if gotEnd != tt.wantEnd {
					t.Errorf("parseRangeHeader() end = %v, want %v", gotEnd, tt.wantEnd)
				}
			}
		})
	}
}

func TestHasPathPermission(t *testing.T) {
	tests := []struct {
		name      string
		userID    string
		key       string
		wantAllow bool
	}{
		{
			name:      "user can access own path",
			userID:    "user123",
			key:       "user-user123/file.txt",
			wantAllow: true,
		},
		{
			name:      "user can access own nested path",
			userID:    "user123",
			key:       "user-user123/path/to/file.txt",
			wantAllow: true,
		},
		{
			name:      "user cannot access other user path",
			userID:    "user123",
			key:       "user-user456/file.txt",
			wantAllow: false,
		},
		{
			name:      "user cannot access root path",
			userID:    "user123",
			key:       "file.txt",
			wantAllow: false,
		},
		{
			name:      "user cannot access other bucket",
			userID:    "user123",
			key:       "otherbucket/user-user123/file.txt",
			wantAllow: false,
		},
		{
			name:      "partial prefix match not allowed",
			userID:    "user1",
			key:       "user-user123/file.txt",
			wantAllow: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAllow := hasPathPermission(tt.userID, tt.key)
			if gotAllow != tt.wantAllow {
				t.Errorf("hasPathPermission(%q, %q) = %v, want %v", tt.userID, tt.key, gotAllow, tt.wantAllow)
			}
		})
	}
}

func TestMapBackendError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
	}{
		{
			name:     "key not found error",
			err:      rtcoss3.ErrBackendKeyNotFound,
			wantCode: "NoSuchKey",
		},
		{
			name:     "wrapped key not found error",
			err:      fmt.Errorf("minio get_object: %w", rtcoss3.ErrBackendKeyNotFound),
			wantCode: "NoSuchKey",
		},
		{
			name:     "access denied error",
			err:      rtcoss3.ErrBackendAccessDenied,
			wantCode: "AccessDenied",
		},
		{
			name:     "wrapped access denied error",
			err:      fmt.Errorf("minio put_object: %w", rtcoss3.ErrBackendAccessDenied),
			wantCode: "AccessDenied",
		},
		{
			name:     "bucket not found error",
			err:      rtcoss3.ErrBackendBucketNotFound,
			wantCode: "NoSuchBucket",
		},
		{
			name:     "insufficient storage error",
			err:      rtcoss3.ErrBackendInsufficientStorage,
			wantCode: "InsufficientStorage",
		},
		{
			name:     "generic error",
			err:      rtcoss3.ErrInternalError,
			wantCode: "InternalError",
		},
		{
			name:     "nil error",
			err:      nil,
			wantCode: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapBackendError(tt.err)
			if tt.err == nil {
				if got != nil {
					t.Errorf("mapBackendError(nil) = %v, want nil", got)
				}
				return
			}
			if got.Code != tt.wantCode {
				t.Errorf("mapBackendError() code = %v, want %v", got.Code, tt.wantCode)
			}
		})
	}
}

func TestIsMultipartRequest(t *testing.T) {
	tests := []struct {
		name          string
		query         string
		wantMultipart bool
	}{
		{
			name:          "create multipart upload",
			query:         "uploads=",
			wantMultipart: true,
		},
		{
			name:          "upload part",
			query:         "uploadId=abc123&partNumber=1",
			wantMultipart: true,
		},
		{
			name:          "complete multipart upload",
			query:         "uploadId=abc123",
			wantMultipart: true,
		},
		{
			name:          "abort multipart upload",
			query:         "uploadId=abc123",
			wantMultipart: true,
		},
		{
			name:          "list parts",
			query:         "uploadId=abc123",
			wantMultipart: true,
		},
		{
			name:          "regular get object",
			query:         "",
			wantMultipart: false,
		},
		{
			name:          "list objects",
			query:         "prefix=test",
			wantMultipart: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/bucket/key?"+tt.query, nil)
			got := isMultipartRequest(req)
			if got != tt.wantMultipart {
				t.Errorf("isMultipartRequest() = %v, want %v", got, tt.wantMultipart)
			}
		})
	}
}

func TestExtractUserIDFromContext(t *testing.T) {
	tests := []struct {
		name       string
		ctx        context.Context
		wantUserID string
	}{
		{
			name:       "user id present",
			ctx:        context.WithValue(context.Background(), ContextKeyUserID, "user123"),
			wantUserID: "user123",
		},
		{
			name:       "user id missing",
			ctx:        context.Background(),
			wantUserID: "",
		},
		{
			name:       "user id wrong type",
			ctx:        context.WithValue(context.Background(), ContextKeyUserID, 123),
			wantUserID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractUserIDFromContext(tt.ctx)
			if got != tt.wantUserID {
				t.Errorf("ExtractUserIDFromContext() = %v, want %v", got, tt.wantUserID)
			}
		})
	}
}

func TestExtractRequestIDFromContext(t *testing.T) {
	tests := []struct {
		name          string
		ctx           context.Context
		wantRequestID string
	}{
		{
			name:          "request id present",
			ctx:           context.WithValue(context.Background(), ContextKeyRequestID, "req123"),
			wantRequestID: "req123",
		},
		{
			name:          "request id missing",
			ctx:           context.Background(),
			wantRequestID: "",
		},
		{
			name:          "request id wrong type",
			ctx:           context.WithValue(context.Background(), ContextKeyRequestID, 123),
			wantRequestID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractRequestIDFromContext(tt.ctx)
			if got != tt.wantRequestID {
				t.Errorf("ExtractRequestIDFromContext() = %v, want %v", got, tt.wantRequestID)
			}
		})
	}
}

func TestExtractOperation(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		query      string
		copySource string
		wantOp     string
	}{
		{
			name:   "put object",
			method: "PUT",
			path:   "/bucket/key",
			wantOp: "PutObject",
		},
		{
			name:       "copy object",
			method:     "PUT",
			path:       "/bucket/key",
			copySource: "/source-bucket/source-key",
			wantOp:     "CopyObject",
		},
		{
			name:   "get object",
			method: "GET",
			path:   "/bucket/key",
			wantOp: "GetObject",
		},
		{
			name:   "delete object",
			method: "DELETE",
			path:   "/bucket/key",
			wantOp: "DeleteObject",
		},
		{
			name:   "head object",
			method: "HEAD",
			path:   "/bucket/key",
			wantOp: "HeadObject",
		},
		{
			name:   "list objects",
			method: "GET",
			path:   "/bucket",
			wantOp: "ListObjects",
		},
		{
			name:   "create multipart upload",
			method: "POST",
			path:   "/bucket/key",
			query:  "uploads=",
			wantOp: "CreateMultipartUpload",
		},
		{
			name:   "upload part",
			method: "PUT",
			path:   "/bucket/key",
			query:  "uploadId=abc&partNumber=1",
			wantOp: "UploadPart",
		},
		{
			name:   "complete multipart upload",
			method: "POST",
			path:   "/bucket/key",
			query:  "uploadId=abc",
			wantOp: "CompleteMultipartUpload",
		},
		{
			name:   "abort multipart upload",
			method: "DELETE",
			path:   "/bucket/key",
			query:  "uploadId=abc",
			wantOp: "AbortMultipartUpload",
		},
		{
			name:   "list parts",
			method: "GET",
			path:   "/bucket/key",
			query:  "uploadId=abc",
			wantOp: "ListParts",
		},
		{
			name:   "unknown operation",
			method: "PATCH",
			path:   "/bucket/key",
			wantOp: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqURL, _ := url.Parse("http://example.com" + tt.path)
			if tt.query != "" {
				reqURL.RawQuery = tt.query
			}
			req := &http.Request{
				Method: tt.method,
				URL:    reqURL,
				Header: make(http.Header),
			}
			if tt.copySource != "" {
				req.Header.Set("X-Amz-Copy-Source", tt.copySource)
			}
			got := extractOperation(req)
			if got != tt.wantOp {
				t.Errorf("extractOperation() = %v, want %v", got, tt.wantOp)
			}
		})
	}
}
