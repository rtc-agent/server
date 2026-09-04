package updates

import (
	"github.com/rtc-agent/server/internal/dbmodel"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ========== dbmodel → protocol 转换 ==========
//
// 供 UpdatePublisher 内部使用：将 repo 返回的 dbmodel 实体转换为 protocol DTO，
// 用于组装推送到 Centrifuge 的富内容 Update。
//
// Session 转换已迁移到 dbmodel.ToProtocolSession，供多处复用。

// toProtocolSession 调用 dbmodel.ToProtocolSession，保留本包内的短名称便于内部使用。
func toProtocolSession(m *dbmodel.Session) protocol.Session {
	return dbmodel.ToProtocolSession(m)
}

func toProtocolTurn(m *dbmodel.Turn) protocol.Turn {
	return dbmodel.ToProtocolTurn(m)
}

func toProtocolMessage(m *dbmodel.Message) protocol.Message {
	return dbmodel.ToProtocolMessage(m)
}

func toProtocolRtc(m *dbmodel.Rtc) protocol.Rtc {
	return dbmodel.ToProtocolRtc(m)
}

// ========== 通用辅助 ==========

// DerefUpdates 将 []*protocol.Update 转为 *[]protocol.Update。
// handler 层用于组装 RPC 响应的 Updates 字段。
func DerefUpdates(src []*protocol.Update) *[]protocol.Update {
	if src == nil {
		return nil
	}
	out := make([]protocol.Update, len(src))
	for i, u := range src {
		out[i] = *u
	}
	return &out
}
