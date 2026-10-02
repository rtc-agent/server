package primitives

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// MaxFilesPerMessage is the maximum number of file attachments allowed per message.
// Aligned with reference project's APIMaxMediaPerRequest (apiLimits.ts:94).
// Prevents malicious clients from sending excessive file references that would
// cause large DB queries via KeysExist (which batches in groups of 1000).
const MaxFilesPerMessage = 100

// fileIDPattern matches: {md5-hash}.{ext}
// - md5-hash: 32 character hex string (lowercase a-f or digits)
// - ext: file extension (1-10 alphanumeric characters)
var fileIDPattern = regexp.MustCompile(`^[a-f0-9]{32}\.[a-zA-Z0-9]{1,10}$`)

// ValidateFilesExist verifies that all referenced files exist in the database
// and belong to the specified user.
//
// File ID format: {md5}.{ext} (e.g., "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6.txt")
// The server constructs the full S3 key as: user-{userID}/{fileID}
//
// Security: We validate that each file ID matches the expected format and
// construct the full key to prevent cross-user file references.
//
// Returns nil if files is nil or empty (no validation needed).
// Returns an error if:
//   - len(files) > MaxFilesPerMessage (too many files)
//   - any file ID has invalid format
//   - any file is not found in the database
func ValidateFilesExist(ctx context.Context, fileRepo repo.FileRepo, files []protocol.FileAttachment, userID uuid.UUID) error {
	if len(files) == 0 {
		return nil
	}

	// Enforce file count limit to prevent excessive DB queries
	if len(files) > MaxFilesPerMessage {
		return fmt.Errorf("too many file attachments: %d exceeds maximum %d", len(files), MaxFilesPerMessage)
	}

	keys := make([]string, 0, len(files))

	for _, f := range files {
		// Validate file ID format: {md5}.{ext}
		if !fileIDPattern.MatchString(f.Fileid) {
			return fmt.Errorf("invalid file ID format: %q (expected {md5}.{ext})", f.Fileid)
		}
		// Construct full S3 key: user-{userID}/{fileID}
		fullKey := rtcoss3.BuildFileKey(userID.String(), f.Fileid)
		keys = append(keys, fullKey)
	}

	exists, err := fileRepo.KeysExist(ctx, keys)
	if err != nil {
		return fmt.Errorf("file verification failed: %w", err)
	}

	var missing []string
	for _, key := range keys {
		if !exists[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("files not found: %s", strings.Join(missing, ", "))
	}
	return nil
}
