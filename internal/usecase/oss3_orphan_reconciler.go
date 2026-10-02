package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.uber.org/zap"
)

// OrphanReconcileResult holds the outcome of an orphan reconciliation cycle.
type OrphanReconcileResult struct {
	// ObjectsScanned is the total number of MinIO objects examined.
	ObjectsScanned int
	// OrphansFound is the number of objects with no matching DB record (before cooldown filter).
	OrphansFound int
	// OrphansDeleted is the number of orphan objects actually deleted (0 in dry-run mode).
	OrphansDeleted int
	// OrphansSkipped is the number of orphan objects skipped due to cooldown period.
	OrphansSkipped int
	// BytesFreed is the total size of deleted orphan objects.
	BytesFreed int64
	// Duration is the wall-clock time taken by the reconciliation.
	Duration time.Duration
}

// ReconcileOrphans scans MinIO for objects that have no corresponding DB record
// and deletes them. This is the safety net for DB failures that survive the
// immediate compensation in CompleteMultipartUploadWithComp / PutObjectWithComp.
//
// Algorithm:
//  1. List objects from MinIO in batches (paginated by key).
//  2. For each batch, check which keys exist in the DB via KeysExist.
//  3. Objects present in MinIO but absent from DB are candidates for deletion.
//  4. Candidates younger than the cooldown period are skipped (may still be uploading).
//  5. Remaining candidates are deleted (or logged in dry-run mode).
//
// Performance:
//   - Pagination ensures bounded memory regardless of bucket size.
//   - Cooldown prevents racing with in-flight uploads.
//   - Distributed lock ensures only one node runs reconciliation at a time.
func (uc *OSS3Usecase) ReconcileOrphans(ctx context.Context) (*OrphanReconcileResult, error) {
	start := time.Now()
	cfg := uc.cfg.Cleanup.Orphan

	if !cfg.Enabled {
		logger.Debug(ctx, "orphan reconciler: disabled, skipping")
		return &OrphanReconcileResult{Duration: time.Since(start)}, nil
	}

	// Verify Redis and scripts are available for distributed locking.
	if uc.redis == nil || uc.scripts == nil {
		return nil, fmt.Errorf("orphan reconciler: Redis or scripts not configured")
	}

	// Acquire distributed lock to prevent concurrent reconciliation across nodes.
	const lockTTL = 10 * time.Minute
	holderUUID, acquired, err := uc.acquireOrphanLock(ctx, lockTTL)
	if err != nil {
		return nil, fmt.Errorf("acquire orphan lock: %w", err)
	}
	if !acquired {
		logger.Debug(ctx, "orphan reconciler: lock held by another node, skipping")
		return &OrphanReconcileResult{Duration: time.Since(start)}, nil
	}
	defer uc.releaseOrphanLock(ctx, holderUUID)

	bucket := uc.cfg.MinIO.Bucket
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 500
	}
	cooldown := cfg.CooldownPeriod
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	dryRun := cfg.DryRun

	result := &OrphanReconcileResult{}
	marker := ""
	cooldownThreshold := time.Now().Add(-cooldown)

	for {
		// Extend lock before each page.
		if extendErr := uc.extendOrphanLock(ctx, holderUUID, lockTTL); extendErr != nil {
			return nil, fmt.Errorf("extend orphan lock: %w", extendErr)
		}

		// List objects from MinIO.
		listResult, listErr := uc.backend.ListObjects(ctx, bucket, rtcoss3.ListObjectsOptions{
			MaxKeys: batchSize,
			Marker:  marker,
			// No Delimiter = recursive listing (all objects, no prefix grouping)
		})
		if listErr != nil {
			return nil, fmt.Errorf("list objects (marker=%q): %w", marker, listErr)
		}

		if len(listResult.Objects) == 0 {
			break
		}

		result.ObjectsScanned += len(listResult.Objects)

		// Extract keys for batch lookup.
		keys := make([]string, 0, len(listResult.Objects))
		keyMeta := make(map[string]rtcoss3.ObjectMeta, len(listResult.Objects))
		for _, obj := range listResult.Objects {
			keys = append(keys, obj.Key)
			keyMeta[obj.Key] = obj
		}

		// Check which keys exist in DB.
		existingKeys, dbErr := uc.fileRepo.KeysExist(ctx, keys)
		if dbErr != nil {
			return nil, fmt.Errorf("keys exist check: %w", dbErr)
		}

		// Identify orphans: keys in MinIO but not in DB.
		orphanKeys := uc.identifyOrphanKeys(ctx, keys, existingKeys, keyMeta, cooldownThreshold, result)

		// Delete orphans.
		uc.deleteOrphansInBatch(ctx, bucket, orphanKeys, keyMeta, dryRun, result)

		// Advance pagination marker.
		newMarker, shouldContinue := advancePaginationMarker(listResult)
		if !shouldContinue {
			break
		}
		marker = newMarker

		// Check context cancellation between pages.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("orphan reconcile: context cancelled: %w", ctx.Err())
		}
	}

	result.Duration = time.Since(start)
	uc.logOrphanReconcileResult(ctx, result, dryRun)

	return result, nil
}

// deleteOrphanBatch deletes a batch of orphan objects from MinIO.
// Returns the count of successfully deleted objects and their total size.
func (uc *OSS3Usecase) deleteOrphanBatch(
	ctx context.Context,
	bucket string,
	keys []string,
	keyMeta map[string]rtcoss3.ObjectMeta,
) (int, int64, error) {
	results, err := uc.backend.DeleteObjects(ctx, bucket, keys)
	if err != nil {
		return 0, 0, fmt.Errorf("delete orphan batch: %w", err)
	}

	deleted := 0
	var bytesFreed int64
	for _, r := range results {
		if r.Code == "" {
			deleted++
			if meta, ok := keyMeta[r.Key]; ok {
				bytesFreed += meta.Size
			}
		} else {
			logger.Warn(ctx, "orphan reconciler: failed to delete orphan",
				zap.String("key", r.Key),
				zap.String("code", r.Code),
				zap.String("message", r.Message))
		}
	}

	if deleted > 0 {
		logger.Info(ctx, "orphan reconciler: deleted orphan batch",
			zap.Int("deleted", deleted),
			zap.Int64("bytes_freed", bytesFreed))
	}

	return deleted, bytesFreed, nil
}

// identifyOrphanKeys identifies orphan keys from a batch and updates result counters.
// Returns the list of orphan keys that should be deleted (after applying cooldown).
func (uc *OSS3Usecase) identifyOrphanKeys(
	ctx context.Context,
	keys []string,
	existingKeys map[string]bool,
	keyMeta map[string]rtcoss3.ObjectMeta,
	cooldownThreshold time.Time,
	result *OrphanReconcileResult,
) []string {
	var orphanKeys []string
	for _, key := range keys {
		if !existingKeys[key] {
			obj := keyMeta[key]
			result.OrphansFound++

			// Apply cooldown: skip recently created objects.
			if obj.LastModified.After(cooldownThreshold) {
				result.OrphansSkipped++
				logger.Debug(ctx, "orphan reconciler: skipping recent object (cooldown)",
					zap.String("key", key),
					zap.Time("last_modified", obj.LastModified))
				continue
			}

			orphanKeys = append(orphanKeys, key)
		}
	}
	return orphanKeys
}

// deleteOrphansInBatch handles deletion of orphan keys (dry-run or real).
func (uc *OSS3Usecase) deleteOrphansInBatch(
	ctx context.Context,
	bucket string,
	orphanKeys []string,
	keyMeta map[string]rtcoss3.ObjectMeta,
	dryRun bool,
	result *OrphanReconcileResult,
) {
	if len(orphanKeys) == 0 {
		return
	}

	if dryRun {
		for _, key := range orphanKeys {
			obj := keyMeta[key]
			logger.Warn(ctx, "orphan reconciler: [DRY-RUN] would delete orphan",
				zap.String("key", key),
				zap.Int64("size", obj.Size),
				zap.Time("last_modified", obj.LastModified))
			result.BytesFreed += obj.Size
		}
		result.OrphansDeleted += len(orphanKeys)
	} else {
		deleted, bytesFreed, delErr := uc.deleteOrphanBatch(ctx, bucket, orphanKeys, keyMeta)
		if delErr != nil {
			logger.Error(ctx, "orphan reconciler: batch delete failed",
				zap.Int("batch_size", len(orphanKeys)),
				zap.Error(delErr))
			// Continue to next batch; partial progress is acceptable.
		}
		result.OrphansDeleted += deleted
		result.BytesFreed += bytesFreed
	}
}

// advancePaginationMarker computes the next marker for pagination.
// Returns the new marker and whether pagination should continue.
func advancePaginationMarker(listResult *rtcoss3.ListObjectsResult) (string, bool) {
	if !listResult.IsTruncated {
		return "", false
	}
	marker := listResult.NextMarker
	if marker == "" {
		// Fallback: use last key as marker.
		marker = listResult.Objects[len(listResult.Objects)-1].Key
	}
	return marker, true
}

// logOrphanReconcileResult logs the final reconciliation result.
func (uc *OSS3Usecase) logOrphanReconcileResult(ctx context.Context, result *OrphanReconcileResult, dryRun bool) {
	if result.OrphansFound > 0 {
		if dryRun {
			logger.Warn(ctx, "orphan reconciler: cycle complete (dry-run, no objects deleted)",
				zap.Int("objects_scanned", result.ObjectsScanned),
				zap.Int("orphans_found", result.OrphansFound),
				zap.Int("orphans_skipped", result.OrphansSkipped),
				zap.Int64("bytes_freed", result.BytesFreed),
				zap.Duration("duration", result.Duration))
		} else {
			logger.Info(ctx, "orphan reconciler: cycle complete",
				zap.Int("objects_scanned", result.ObjectsScanned),
				zap.Int("orphans_found", result.OrphansFound),
				zap.Int("orphans_deleted", result.OrphansDeleted),
				zap.Int("orphans_skipped", result.OrphansSkipped),
				zap.Int64("bytes_freed", result.BytesFreed),
				zap.Duration("duration", result.Duration))
		}
	} else {
		logger.Debug(ctx, "orphan reconciler: cycle complete, no orphans",
			zap.Int("objects_scanned", result.ObjectsScanned),
			zap.Duration("duration", result.Duration))
	}
}

// acquireOrphanLock acquires the distributed lock for orphan reconciliation.
func (uc *OSS3Usecase) acquireOrphanLock(ctx context.Context, ttl time.Duration) (string, bool, error) {
	return uc.acquireLock(ctx, "orphan_reconcile", ttl)
}

// extendOrphanLock extends the TTL of the orphan reconciliation lock.
func (uc *OSS3Usecase) extendOrphanLock(ctx context.Context, holderUUID string, ttl time.Duration) error {
	return uc.extendLock(ctx, "orphan_reconcile", holderUUID, ttl)
}

// releaseOrphanLock releases the orphan reconciliation lock.
func (uc *OSS3Usecase) releaseOrphanLock(ctx context.Context, holderUUID string) {
	uc.releaseLock(ctx, "orphan_reconcile", holderUUID)
}
