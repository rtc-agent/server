package rtcqueue

import (
	"crypto/sha256"
	"fmt"

	"github.com/google/uuid"
)

// GenerateDeterministicWorkID generates a deterministic work ID based on
// session ID and a unique key (e.g., message ID). This ensures that retries
// of the same logical work item produce the same work ID, preventing
// duplicate work items from being created.
//
// The generated ID is a UUID v5-like identifier derived from the concatenation
// of session ID and unique key, using SHA-256 for better distribution.
//
// Example:
//
//	workID := GenerateDeterministicWorkID(sessionID, messageID)
//	// Retries with the same sessionID + messageID will produce the same workID
func GenerateDeterministicWorkID(sessionID, uniqueKey string) string {
	// Concatenate session ID and unique key with a separator
	input := fmt.Sprintf("%s:%s", sessionID, uniqueKey)

	// Generate SHA-256 hash
	hash := sha256.Sum256([]byte(input))

	// Use first 16 bytes to create a UUID v5-like ID
	// We use SHA-256 instead of SHA-1 for better distribution, but truncate to 16 bytes
	var uuidBytes [16]byte
	copy(uuidBytes[:], hash[:16])

	// Set version (5) and variant (RFC 4122) bits
	uuidBytes[6] = (uuidBytes[6] & 0x0f) | 0x50 // Version 5
	uuidBytes[8] = (uuidBytes[8] & 0x3f) | 0x80 // Variant RFC 4122

	return uuid.UUID(uuidBytes).String()
}
