// Package updates provides HistoryStore implementation for Topic channel offline recovery.
package updates

import (
	"encoding/json"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/centrifugal/centrifuge"
)

// fillGapPublications 将 [sinceOffset+1, latestOffset] 范围内的缺口填充为 gap 占位 publication，
// 保证恢复结果满足 centrifuge 的连续性检查：首条 pub.Offset == sinceOffset+1、末条 pub.Offset == latestOffset。
// 数据被清理/裁剪导致历史不完整时，客户端收到 gap 只推进本地 offset，不做业务处理。
func fillGapPublications(sinceOffset uint32, latestOffset uint32, pubs []*centrifuge.Publication) []*centrifuge.Publication {
	latest := uint64(latestOffset)
	if latest == uint64(sinceOffset) && len(pubs) == 0 {
		return pubs
	}
	out := make([]*centrifuge.Publication, 0, len(pubs)+2)
	cur := uint64(sinceOffset) + 1
	for _, p := range pubs {
		// 为 [cur, p.Offset-1] 范围内的每个缺失 offset 生成一个 gap
		for p.Offset > cur {
			out = append(out, makeGapPublication(uint32(cur)))
			cur++
		}
		out = append(out, p)
		if p.Offset+1 > cur {
			cur = p.Offset + 1
		}
	}
	// 尾部缺口：为 [cur, latest] 范围内的每个缺失 offset 生成一个 gap
	for cur <= latest {
		out = append(out, makeGapPublication(uint32(cur)))
		cur++
	}
	return out
}

// makeGapPublication 构造一条 gap 占位 publication，offset 为缺口起点。
func makeGapPublication(from uint32) *centrifuge.Publication {
	type gapPayload struct {
		Type string                 `json:"type"`
		Data protocol.UpdateDataGap `json:"data"`
	}
	data, _ := json.Marshal(gapPayload{
		Type: string(protocol.UpdateTypeGap),
		Data: protocol.UpdateDataGap{},
	})
	return &centrifuge.Publication{
		Data:   data,
		Offset: uint64(from),
	}
}
