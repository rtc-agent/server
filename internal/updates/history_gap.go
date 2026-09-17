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
// When data is cleaned/trimmed causing incomplete history, the client receives
// gaps and only advances the local offset without business processing.
func fillGapPublications(sinceOffset uint32, latestOffset uint32, pubs []*centrifuge.Publication) []*centrifuge.Publication {
	latest := uint64(latestOffset)
	if latest == uint64(sinceOffset) && len(pubs) == 0 {
		return pubs
	}
	out := make([]*centrifuge.Publication, 0, len(pubs)+2)
	cur := uint64(sinceOffset) + 1
	for _, p := range pubs {
		// Generate a gap for each missing offset in [cur, p.Offset-1]
		for p.Offset > cur {
			out = append(out, makeGapPublication(uint32(cur)))
			cur++
		}
		out = append(out, p)
		if p.Offset+1 > cur {
			cur = p.Offset + 1
		}
	}
	// Tail gap: generate a gap for each missing offset in [cur, latest]
	for cur <= latest {
		out = append(out, makeGapPublication(uint32(cur)))
		cur++
	}
	return out
}

// makeGapPublication constructs a gap placeholder publication with offset
// set to the gap start point.
func makeGapPublication(from uint32) *centrifuge.Publication {
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
		logger.Error(context.Background(), "makeGapPublication: failed to marshal gap payload", zap.Error(err))
		data = []byte(`{"type":"gap","data":{}}`)
	}
	return &centrifuge.Publication{
		Data:   data,
		Offset: uint64(from),
	}
}
