// Package updates provides HistoryStore implementation for Topic channel offline recovery.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"

	"github.com/google/uuid"

	"github.com/centrifugal/centrifuge"
)

// Query 查询指定频道中从指定 offset 开始的 user_update 记录。
func (u *UpdatePublisher) Query(ctx context.Context, ch string, sinceOffset uint32, latestOffset uint32) ([]*centrifuge.Publication, error) {
	userIDStr, ok := channel.ParseUser(ch)
	if !ok {
		return nil, fmt.Errorf("invalid channel format: %s", ch)
	}

	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid user ID in channel: %s", ch)
	}

	var updates []model.UserUpdate
	err = repo.DBFromContext(ctx, u.db).WithContext(ctx).
		Where("user_id = ? AND \"offset\" > ?", userID, sinceOffset).
		Order("\"offset\" ASC").
		Find(&updates).Error
	if err != nil {
		return nil, fmt.Errorf("query user updates: %w", err)
	}

	if len(updates) == 0 {
		return fillGapPublications(sinceOffset, latestOffset, nil), nil
	}

	pubs := make([]*centrifuge.Publication, 0, len(updates))
	for _, update := range updates {
		pubsForUpdate, err := u.buildPublications(ctx, update)
		if err != nil {
			continue
		}
		pubs = append(pubs, pubsForUpdate...)
	}

	return fillGapPublications(sinceOffset, latestOffset, pubs), nil
}

// buildPublications 将 UserUpdate 转换为 centrifuge.Publication。
func (u *UpdatePublisher) buildPublications(ctx context.Context, uu model.UserUpdate) ([]*centrifuge.Publication, error) {
	if len(uu.Items) == 0 {
		return nil, nil
	}

	updates, err := u.convertUpdates(ctx, []*model.UserUpdate{&uu})
	if err != nil {
		return nil, fmt.Errorf("convert updates: %w", err)
	}

	if len(updates) == 0 {
		return nil, nil
	}

	var pubs []*centrifuge.Publication
	for _, update := range updates {
		payload, err := json.Marshal(update)
		if err != nil {
			return nil, fmt.Errorf("marshal update: %w", err)
		}

		pubs = append(pubs, &centrifuge.Publication{
			Data:   payload,
			Offset: uint64(update.Offset),
		})
	}

	return pubs, nil
}
