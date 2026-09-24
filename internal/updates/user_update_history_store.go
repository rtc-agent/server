// Package updates provides HistoryStore implementation for Topic channel offline recovery.
package updates

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"

	"github.com/google/uuid"

	"github.com/centrifugal/centrifuge"
)

// Query returns user_update publications from the given channel starting at sinceOffset.
// Entities are batch-resolved by type to avoid N+1 queries: N updates require only
// E DB queries (E = number of entity types), regardless of how many updates there are.
// limit specifies the maximum number of publications to return (0 means no limit).
func (u *UpdatePublisher) Query(ctx context.Context, ch string, sinceOffset uint32, latestOffset uint32, limit int) ([]*centrifuge.Publication, error) {
	userIDStr, ok := channel.ParseUser(ch)
	if !ok {
		return nil, fmt.Errorf("invalid channel format: %s", ch)
	}

	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid user ID in channel: %s", ch)
	}

	query := repo.DBFromContext(ctx, u.db).WithContext(ctx).
		Where("user_id = ? AND \"offset\" > ?", userID, sinceOffset).
		Order("\"offset\" ASC")

	// Apply limit if specified (0 means no limit).
	if limit > 0 {
		query = query.Limit(limit)
	}

	var updates []model.UserUpdate
	err = query.Find(&updates).Error
	if err != nil {
		return nil, fmt.Errorf("query user updates: %w", err)
	}

	if len(updates) == 0 {
		// No data, but if no limit, fill gaps up to latestOffset
		if limit <= 0 {
			return fillGapPublications(ctx, sinceOffset, latestOffset, nil), nil
		}
		// With limit, just return empty (client will request next batch)
		return nil, nil
	}

	// Convert []model.UserUpdate to []*model.UserUpdate for batch processing.
	// This avoids N+1 queries: convertUpdates collects all entity IDs, resolves
	// them in bulk by entity type, then fills in the resolved data. This requires
	// only E DB queries regardless of the number of updates (E = entity types).
	updatePtrs := make([]*model.UserUpdate, len(updates))
	for i := range updates {
		updatePtrs[i] = &updates[i]
	}

	// Batch convert all updates at once to avoid N+1 queries.
	allUpdates, err := u.convertUpdates(ctx, updatePtrs)
	if err != nil {
		return nil, fmt.Errorf("convert updates: %w", err)
	}

	pubs := make([]*centrifuge.Publication, 0, len(allUpdates))
	for _, update := range allUpdates {
		payload, err := json.Marshal(update)
		if err != nil {
			// A single marshal failure should not abort the entire query;
			// log the error for diagnosis and continue.
			logger.Error(ctx, "marshal update failed",
				zap.String("update_id", update.Id),
				zap.Error(err))
			continue
		}

		pubs = append(pubs, &centrifuge.Publication{
			Data:   payload,
			Offset: uint64(update.Offset),
		})
	}

	// When limit is specified, don't fill tail gaps - client will request next batch.
	// When no limit, fill gaps up to latestOffset for backward compatibility.
	if limit <= 0 {
		return fillGapPublications(ctx, sinceOffset, latestOffset, pubs), nil
	}

	// Fill gaps only within the returned data range (no tail gap)
	if len(pubs) > 0 {
		firstOffset := pubs[0].Offset
		lastOffset := pubs[len(pubs)-1].Offset
		return fillGapPublications(ctx, uint32(firstOffset)-1, uint32(lastOffset), pubs), nil //nolint:gosec // offset fits in uint32
	}
	return pubs, nil
}
