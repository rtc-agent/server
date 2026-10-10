// Package updates provides HistoryStore implementation for Topic channel offline recovery.
package updates

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/centrifugal/centrifuge"
)

// fillGapPublications fills gaps in the range [sinceOffset+1, latestOffset]
// with gap placeholder publications, ensuring the recovery result satisfies
// centrifuge's continuity checks: first pub.Offset == sinceOffset+1,
// last pub.Offset == latestOffset.
//
// Design notes:
//   - Gap filling ensures clients can detect and handle missing history gracefully
//   - maxGapFill (1000) prevents excessive memory allocation when offset difference
//     is large (e.g., due to data cleanup or long disconnection)
//   - Truncation with warning allows partial recovery instead of complete failure
//   - Pre-allocation with estimated capacity minimizes slice reallocations
//
// When data is cleaned/trimmed causing incomplete history, the client receives
// gaps and only advances the local offset without business processing.
func fillGapPublications(ctx context.Context, sinceOffset uint32, latestOffset uint32, pubs []*centrifuge.Publication) []*centrifuge.Publication {
	latest := uint64(latestOffset)
	since := uint64(sinceOffset)

	if latest == since && len(pubs) == 0 {
		return pubs
	}

	// Prevent excessive memory allocation when offset difference is large.
	// If the gap is too large (e.g., due to data cleanup), truncate to maxGapFill
	// to avoid creating millions of gap objects.
	const maxGapFill = 1000
	if latest-since > uint64(maxGapFill) {
		logger.Warn(ctx, "gap_fill_truncated",
			zap.Uint32("since_offset", sinceOffset),
			zap.Uint32("latest_offset", latestOffset),
			zap.Int("max_fill", maxGapFill))
		latest = since + uint64(maxGapFill)
	}

	// Pre-allocate with a more precise capacity: existing pubs + estimated gap count.
	// The gap count is bounded by (latest - since - len(pubs)), since each pub fills at least one slot.
	// Adding 2 provides a small buffer for edge cases (head/tail gaps).
	estimatedGaps := int(latest-since) - len(pubs)
	if estimatedGaps < 0 {
		estimatedGaps = 0
	}
	out := make([]*centrifuge.Publication, 0, len(pubs)+estimatedGaps+2)
	cur := since + 1
	for _, p := range pubs {
		// Generate a gap for each missing offset in [cur, p.Offset-1]
		for p.Offset > cur {
			out = append(out, makeGapPublication(ctx, uint32(cur)))
			cur++
		}
		out = append(out, p)
		if p.Offset+1 > cur {
			cur = p.Offset + 1
		}
	}
	// Tail gap: generate a gap for each missing offset in [cur, latest]
	for cur <= latest {
		out = append(out, makeGapPublication(ctx, uint32(cur)))
		cur++
	}
	return out
}

// makeGapPublication constructs a gap placeholder publication with offset
// set to the gap start point.
func makeGapPublication(ctx context.Context, from uint32) *centrifuge.Publication {
	type gapPayload struct {
		Type string                 `json:"type"`
		Data protocol.UpdateDataGap `json:"data"`
	}
	data, err := json.Marshal(gapPayload{
		Type: string(protocol.UpdateTypeGap),
		Data: protocol.UpdateDataGap{},
	})
	if err != nil {
		// gapPayload only contains string + empty struct, marshal should
		// theoretically never fail; log for debugging if it does.
		logger.Error(ctx, "makeGapPublication: failed to marshal gap payload", zap.Error(err))
		data = []byte(`{"type":"gap","data":{}}`)
	}
	return &centrifuge.Publication{
		Data:   data,
		Offset: uint64(from),
	}
}
