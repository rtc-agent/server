package centrifugeplus

import (
	"context"

	"github.com/centrifugal/centrifuge"
)

// HistoryStore is the interface for querying message history.
// Implementations are provided by the integrator.
type HistoryStore interface {
	// Query retrieves publications from the given channel since the specified offset.
	// latestOffset is the current stream's latest offset, used by fillGapPublications to fill the tail gap,
	// ensuring the recovery result satisfies centrifuge's continuity check (last pub.Offset == latestOffset).
	Query(ctx context.Context, channel string, sinceOffset uint32, latestOffset uint32) ([]*centrifuge.Publication, error)
}

// HistoryStoreRemover is an optional interface that HistoryStore implementations
// can implement to support removing history for a channel.
type HistoryStoreRemover interface {
	// RemoveHistory removes all history for the given channel.
	RemoveHistory(channel string) error
}
