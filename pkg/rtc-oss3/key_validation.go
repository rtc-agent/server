package rtcoss3

import (
	"fmt"
	"regexp"
	"strings"
)

// keyPattern matches: {user-id}/{md5-hash}.{ext}
// - user-id: starts with "user-" followed by UUID
// - md5-hash: 32 character hex string
// - ext: file extension (1-10 alphanumeric characters)
var keyPattern = regexp.MustCompile(`^user-[a-f0-9-]{36}/[a-f0-9]{32}\.[a-zA-Z0-9]{1,10}$`)

// ValidateKey validates that a key follows the format: {user-id}/{md5-hash}.{ext}
// Returns nil if valid, or an S3Error describing the violation.
func ValidateKey(key, userID string) *S3Error {
	// Check overall format
	if !keyPattern.MatchString(key) {
		return ErrInvalidKeyFormat
	}

	// Check user prefix
	expectedPrefix := "user-" + userID + "/"
	if !strings.HasPrefix(key, expectedPrefix) {
		return ErrAccessDenied
	}

	return nil
}

// AllowedContentTypePrefixes lists the Content-Type prefixes permitted for uploads.
// Only image/* and text/* types are allowed.
var AllowedContentTypePrefixes = []string{"image/", "text/"}

// IsAllowedContentType checks if the given Content-Type is in the allowed list.
// Empty Content-Type defaults to "application/octet-stream" which is NOT allowed.
// Content-Type may include parameters (e.g., "text/plain; charset=utf-8");
// only the media type portion (before ';') is checked.
func IsAllowedContentType(contentType string) bool {
	// Strip parameters (e.g., "; charset=utf-8")
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	contentType = strings.ToLower(strings.TrimSpace(contentType))

	for _, prefix := range AllowedContentTypePrefixes {
		if strings.HasPrefix(contentType, prefix) {
			return true
		}
	}
	return false
}

// GenerateKey generates a valid key in the format: {user-id}/{md5-hash}.{ext}
func GenerateKey(userID, md5Hash, ext string) string {
	return fmt.Sprintf("user-%s/%s.%s", userID, md5Hash, ext)
}
