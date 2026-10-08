package rtcoss3

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// ListObjects lists objects.
// MEDIUM-22 fix: When delimiter is set and Recursive=false, the MinIO SDK handles
// common prefix extraction server-side. Objects whose keys end with the delimiter
// are common prefixes (directories); others are regular objects.
// LOW-05 fix: Support pagination via Marker, ContinuationToken, and StartAfter.
// Pagination fix: Use MaxKeys+1 strategy to accurately detect truncation.
// CommonPrefixes count towards the MaxKeys quota (totalKeys tracks both Objects and CommonPrefixes).
//
// Boundary condition handling (H2):
//   - We request maxKeys+1 items from MinIO to detect if there are more items
//   - Both Objects and CommonPrefixes count towards maxKeys limit (per S3 spec)
//   - If we receive maxKeys+1 items, we return maxKeys and set IsTruncated=true
//   - If we receive <= maxKeys items, we return all and set IsTruncated=false
//   - Edge case: if the (maxKeys+1)th item is a CommonPrefix, we correctly detect
//     truncation but don't include it in results (client fetches next page)
func (b *MinIOBackend) ListObjects(ctx context.Context, bucket string, opts ListObjectsOptions) (*ListObjectsResult, error) {
	result := &ListObjectsResult{}

	maxKeys := opts.MaxKeys
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	listOpts := minio.ListObjectsOptions{
		Prefix:    opts.Prefix,
		Recursive: opts.Delimiter == "",
		MaxKeys:   maxKeys + 1, // fetch one extra to detect truncation accurately
	}

	// Determine pagination start point
	// Priority: ContinuationToken (V2) > StartAfter (V2) > Marker (V1)
	if opts.ContinuationToken != "" {
		// V2: ContinuationToken is opaque (base64-encoded), decode it first
		decoded, err := DecodeContinuationToken(opts.ContinuationToken)
		if err != nil {
			return nil, fmt.Errorf("invalid continuation token: %w", err)
		}
		listOpts.StartAfter = decoded
	} else if opts.StartAfter != "" {
		// V2: StartAfter is the key to start after (first request only)
		listOpts.StartAfter = opts.StartAfter
	} else if opts.Marker != "" {
		// V1: Marker is the key to start after
		listOpts.StartAfter = opts.Marker
	}

	// Collect raw items from MinIO SDK channel.
	var rawItems []rawListItem
	for object := range b.client.ListObjects(ctx, bucket, listOpts) {
		if object.Err != nil {
			return nil, mapMinIOError(ctx, object.Err, "list_objects")
		}
		rawItems = append(rawItems, rawListItem{
			Key:          object.Key,
			Size:         object.Size,
			ContentType:  object.ContentType,
			ETag:         object.ETag,
			LastModified: object.LastModified,
		})
	}

	// Apply pagination logic (extracted for unit testing).
	result.Objects, result.CommonPrefixes, result.IsTruncated, result.NextMarker =
		computeListPagination(rawItems, opts.Delimiter, maxKeys)

	if result.IsTruncated && result.NextMarker != "" {
		result.NextContinuationToken = EncodeContinuationToken(result.NextMarker)
	}
	result.KeyCount = len(result.Objects) + len(result.CommonPrefixes)

	return result, nil
}

// rawListItem is a minimal representation of a listed object, used by
// computeListPagination to decouple pagination logic from the MinIO SDK.
type rawListItem struct {
	Key          string
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

// computeListPagination applies the MaxKeys quota and delimiter-based
// CommonPrefix extraction to a sorted slice of raw items. It returns the
// paginated objects, common prefixes, truncation flag, and NextMarker.
//
// This is extracted from ListObjects so the pagination logic can be
// unit-tested without a live MinIO server.
func computeListPagination(items []rawListItem, delimiter string, maxKeys int) (
	objects []ObjectMeta,
	commonPrefixes []string,
	truncated bool,
	nextMarker string,
) {
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	totalKeys := 0
	var lastKey string

	for _, item := range items {
		// Common prefix detection: delimiter set and key ends with delimiter.
		if delimiter != "" && strings.HasSuffix(item.Key, delimiter) {
			if totalKeys >= maxKeys {
				truncated = true
				break
			}
			totalKeys++
			commonPrefixes = append(commonPrefixes, item.Key)
			lastKey = item.Key
			continue
		}

		// Regular object — check quota before adding.
		if totalKeys >= maxKeys {
			truncated = true
			break
		}

		objects = append(objects, ObjectMeta(item))
		totalKeys++
		lastKey = item.Key
	}

	if truncated && lastKey != "" {
		nextMarker = lastKey
	}
	return objects, commonPrefixes, truncated, nextMarker
}
