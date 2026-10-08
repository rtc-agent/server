package rtcoss3

import (
	"encoding/base64"
	"fmt"
)

// ContinuationToken encoding/decoding for ListObjectsV2 pagination.
// The token is base64-encoded last-key, making it opaque to clients.

// EncodeContinuationToken encodes a key into an opaque continuation token.
func EncodeContinuationToken(key string) string {
	return base64.URLEncoding.EncodeToString([]byte(key))
}

// DecodeContinuationToken decodes an opaque continuation token back to a key.
func DecodeContinuationToken(token string) (string, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("invalid continuation token: %w", err)
	}
	return string(decoded), nil
}
