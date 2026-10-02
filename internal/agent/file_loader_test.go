package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rtc-agent/server/pkg/protocol"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// mockOSSBackend implements rtcoss3.Backend for testing.
type mockOSSBackend struct {
	objects map[string][]byte
	getErr  error
}

func (m *mockOSSBackend) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, contentType string) (string, error) {
	return "", nil
}

func (m *mockOSSBackend) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, rtcoss3.ObjectMeta, error) {
	if m.getErr != nil {
		return nil, rtcoss3.ObjectMeta{}, m.getErr
	}
	data, ok := m.objects[key]
	if !ok {
		return nil, rtcoss3.ObjectMeta{}, fmt.Errorf("object not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(data)), rtcoss3.ObjectMeta{}, nil
}

func (m *mockOSSBackend) GetObjectRange(ctx context.Context, bucket, key string, start, end int64) (io.ReadCloser, rtcoss3.ObjectMeta, error) {
	return nil, rtcoss3.ObjectMeta{}, nil
}

func (m *mockOSSBackend) DeleteObject(ctx context.Context, bucket, key string) error {
	return nil
}

func (m *mockOSSBackend) DeleteObjects(ctx context.Context, bucket string, keys []string) ([]rtcoss3.DeleteResult, error) {
	return nil, nil
}

func (m *mockOSSBackend) HeadObject(ctx context.Context, bucket, key string) (rtcoss3.ObjectMeta, error) {
	return rtcoss3.ObjectMeta{}, nil
}

func (m *mockOSSBackend) ListObjects(ctx context.Context, bucket string, opts rtcoss3.ListObjectsOptions) (*rtcoss3.ListObjectsResult, error) {
	return nil, nil
}

func (m *mockOSSBackend) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (rtcoss3.ObjectMeta, error) {
	return rtcoss3.ObjectMeta{}, nil
}

func (m *mockOSSBackend) CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (*rtcoss3.MultipartUploadResult, error) {
	return nil, nil
}

func (m *mockOSSBackend) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (string, error) {
	return "", nil
}

func (m *mockOSSBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []rtcoss3.CompletedPart) (string, error) {
	return "", nil
}

func (m *mockOSSBackend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	return nil
}

func (m *mockOSSBackend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]rtcoss3.PartInfo, error) {
	return nil, nil
}

func (m *mockOSSBackend) PresignGet(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	return "", nil
}

func (m *mockOSSBackend) PresignPut(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	return "", nil
}

func (m *mockOSSBackend) Close() error {
	return nil
}

func (m *mockOSSBackend) HealthCheck(ctx context.Context) error {
	return nil
}

// Ensure mockOSSBackend implements rtcoss3.Backend.
var _ rtcoss3.Backend = (*mockOSSBackend)(nil)

func TestLoadTextFromOSS(t *testing.T) {
	ctx := context.Background()
	bucket := "test-bucket"

	tests := []struct {
		name        string
		content     string
		key         string
		getErr      error
		wantErr     bool
		errContains string
		validate    func(t *testing.T, content string)
	}{
		{
			name:    "normal text file",
			content: "Hello, World!",
			key:     "user-123/test.txt",
			wantErr: false,
			validate: func(t *testing.T, content string) {
				if content != "Hello, World!" {
					t.Errorf("got %q, want %q", content, "Hello, World!")
				}
			},
		},
		{
			name:    "UTF-8 text with Chinese characters",
			content: "你好世界，这是一个测试文件。",
			key:     "user-123/chinese.txt",
			wantErr: false,
			validate: func(t *testing.T, content string) {
				if content != "你好世界，这是一个测试文件。" {
					t.Errorf("got %q, want %q", content, "你好世界，这是一个测试文件。")
				}
			},
		},
		{
			name:        "non-UTF-8 file",
			content:     string([]byte{0xff, 0xfe, 0xfd}), // Invalid UTF-8
			key:         "user-123/invalid.txt",
			wantErr:     true,
			errContains: "not valid UTF-8",
		},
		{
			name:    "large file truncation",
			content: strings.Repeat("a", MaxTextFileSize+100),
			key:     "user-123/large.txt",
			wantErr: false,
			validate: func(t *testing.T, content string) {
				// Should be truncated with marker
				if !strings.HasSuffix(content, TextFileTruncation) {
					t.Errorf("expected truncation marker, got %q", content[len(content)-50:])
				}
				// Verify UTF-8 validity after truncation
				if !utf8.ValidString(content) {
					t.Errorf("truncated content is not valid UTF-8")
				}
			},
		},
		{
			name:    "large file with multi-byte UTF-8 at boundary",
			content: strings.Repeat("a", MaxTextFileSize-2) + "你好", // "你好" is 6 bytes
			key:     "user-123/multibyte.txt",
			wantErr: false,
			validate: func(t *testing.T, content string) {
				// Should truncate safely at rune boundary
				if !utf8.ValidString(content) {
					t.Errorf("truncated content is not valid UTF-8")
				}
			},
		},
		{
			name:        "OSS error",
			content:     "test",
			key:         "user-123/test.txt",
			getErr:      fmt.Errorf("connection timeout"),
			wantErr:     true,
			errContains: "get object from OSS",
		},
		{
			name:        "object not found",
			content:     "",
			key:         "user-123/nonexistent.txt",
			wantErr:     true,
			errContains: "object not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &mockOSSBackend{
				objects: make(map[string][]byte),
				getErr:  tt.getErr,
			}
			if tt.content != "" {
				backend.objects[tt.key] = []byte(tt.content)
			}

			content, err := LoadTextFromOSS(ctx, backend, bucket, tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadTextFromOSS() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %q, want contains %q", err.Error(), tt.errContains)
				}
				return
			}
			if tt.validate != nil {
				tt.validate(t, content)
			}
		})
	}
}

func TestExtractFileName(t *testing.T) {
	tests := []struct {
		name     string
		file     protocol.FileAttachment
		expected string
	}{
		{
			name: "Extra with name",
			file: protocol.FileAttachment{
				Fileid:   "user-123/abc123.txt",
				Mimetype: "text/plain",
				Extra:    &map[string]interface{}{"name": "readme.txt"},
			},
			expected: "readme.txt",
		},
		{
			name: "Extra with filename",
			file: protocol.FileAttachment{
				Fileid:   "user-123/abc123.txt",
				Mimetype: "text/plain",
				Extra:    &map[string]interface{}{"filename": "document.pdf"},
			},
			expected: "document.pdf",
		},
		{
			name: "Extra with both name and filename (name takes priority)",
			file: protocol.FileAttachment{
				Fileid:   "user-123/abc123.txt",
				Mimetype: "text/plain",
				Extra:    &map[string]interface{}{"name": "priority.txt", "filename": "fallback.txt"},
			},
			expected: "priority.txt",
		},
		{
			name: "Extra is nil",
			file: protocol.FileAttachment{
				Fileid:   "user-123/abc123.txt",
				Mimetype: "text/plain",
				Extra:    nil,
			},
			expected: "abc123.txt",
		},
		{
			name: "Extra is empty map",
			file: protocol.FileAttachment{
				Fileid:   "user-123/abc123.txt",
				Mimetype: "text/plain",
				Extra:    &map[string]interface{}{},
			},
			expected: "abc123.txt",
		},
		{
			name: "Extra with empty name string",
			file: protocol.FileAttachment{
				Fileid:   "user-123/abc123.txt",
				Mimetype: "text/plain",
				Extra:    &map[string]interface{}{"name": ""},
			},
			expected: "abc123.txt",
		},
		{
			name: "Extra with non-string name",
			file: protocol.FileAttachment{
				Fileid:   "user-123/abc123.txt",
				Mimetype: "text/plain",
				Extra:    &map[string]interface{}{"name": 123},
			},
			expected: "abc123.txt",
		},
		{
			name: "Fileid without slash",
			file: protocol.FileAttachment{
				Fileid:   "singlename.txt",
				Mimetype: "text/plain",
				Extra:    nil,
			},
			expected: "singlename.txt",
		},
		{
			name: "Fileid with multiple slashes",
			file: protocol.FileAttachment{
				Fileid:   "user-123/subdir/file.txt",
				Mimetype: "text/plain",
				Extra:    nil,
			},
			expected: "file.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractFileName(tt.file)
			if result != tt.expected {
				t.Errorf("ExtractFileName() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestDetectImageFormatFromBuffer(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected string
	}{
		{
			name:     "PNG magic bytes",
			data:     []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x00},
			expected: "image/png",
		},
		{
			name:     "JPEG magic bytes",
			data:     []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01},
			expected: "image/jpeg",
		},
		{
			name:     "GIF magic bytes",
			data:     []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00},
			expected: "image/gif",
		},
		{
			name: "WebP magic bytes",
			data: []byte{
				0x52, 0x49, 0x46, 0x46, // RIFF
				0x24, 0x00, 0x00, 0x00, // file size - 8
				0x57, 0x45, 0x42, 0x50, // WEBP
				0x56, 0x50, 0x38, 0x4C, // VP8L
				0x17, 0x00, 0x00, 0x00, // chunk size
				0x2F, 0x00, 0x00, 0x00, // signature
				0x00, 0x00, 0x00, 0x00, // data
			},
			expected: "image/webp",
		},
		{
			name:     "Too short for detection",
			data:     []byte{0x89, 0x50},
			expected: "text/plain",
		},
		{
			name:     "Unknown format falls back to http.DetectContentType",
			data:     []byte("Hello, World!"),
			expected: "text/plain; charset=utf-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detectImageFormatFromBuffer(tt.data)
			if result != tt.expected {
				t.Errorf("detectImageFormatFromBuffer() = %q, want %q", result, tt.expected)
			}
		})
	}
}
