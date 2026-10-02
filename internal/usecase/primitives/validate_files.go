package primitives

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ValidateFilesExist verifies that all referenced files exist in the database
// and belong to the specified user.
//
// Security: KeysExist does not filter by user_id, so we must validate that each
// file key starts with "user-{userID}/" to prevent user A from referencing user B's files.
//
// Returns nil if files is nil or empty (no validation needed).
// Returns an error if any file is missing or does not belong to the user.
func ValidateFilesExist(ctx context.Context, fileRepo repo.FileRepo, files []protocol.FileAttachment, userID uuid.UUID) error {
	if len(files) == 0 {
		return nil
	}

	keys := make([]string, 0, len(files))
	expectedPrefix := "user-" + userID.String() + "/"

	for _, f := range files {
		// Security check: ensure the key belongs to the current user.
		// This prevents cross-user file references since KeysExist does not filter by user_id.
		if !strings.HasPrefix(f.Fileid, expectedPrefix) {
			return fmt.Errorf("file %q does not belong to user %s", f.Fileid, userID.String())
		}
		keys = append(keys, f.Fileid)
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
