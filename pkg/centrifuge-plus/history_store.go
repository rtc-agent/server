package centrifugeplus

import (
	"context"

	"github.com/centrifugal/centrifuge"
)

// HistoryStore is the interface for querying message history.
// Implementations are provided by the integrator.
type HistoryStore interface {
	// Query retrieves publications from the given channel since the specified offset.
	// latestOffset 是当前 stream 的最新 offset，用于 fillGapPublications 补齐尾部缺口，
	// 保证恢复结果满足 centrifuge 的连续性检查（末条 pub.Offset == latestOffset）。
	Query(ctx context.Context, channel string, sinceOffset uint32, latestOffset uint32) ([]*centrifuge.Publication, error)
}

// HistoryStoreRemover is an optional interface that HistoryStore implementations
// can implement to support removing history for a channel.
type HistoryStoreRemover interface {
	// RemoveHistory removes all history for the given channel.
	RemoveHistory(channel string) error
}
