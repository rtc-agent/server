package updates

import (
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ========== DB model → protocol conversion ==========
//
// Used internally by UpdatePublisher: converts repo-returned dbmodel entities
// to protocol DTOs for assembling rich-content Updates pushed to Centrifuge.
//
// Session conversion has been migrated to model.ToProtocolSession for reuse across packages.

// toProtocolSession calls model.ToProtocolSession, keeping the short name
// for internal convenience.
func toProtocolSession(m *model.Session) protocol.Session {
	return model.ToProtocolSession(m)
}

func toProtocolTurn(m *model.Turn) protocol.Turn {
	return model.ToProtocolTurn(m)
}

func toProtocolMessage(m *model.Message) protocol.Message {
	return model.ToProtocolMessage(m)
}

func toProtocolRtc(m *model.Rtc) protocol.Rtc {
	return model.ToProtocolRtc(m)
}

// ========== Common helpers ==========

// DerefUpdates converts []*protocol.Update to *[]protocol.Update.
// Used by the handler layer to assemble the Updates field in RPC responses.
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

// enrichSessionWithTokenEstimate computes token estimate fields from the
// Session model's persisted EWMA and populates the protocol.Session's
// estimate fields.
// threshold is the compression trigger threshold (contextTokensLimit - autoCompactBufferTokens).
func enrichSessionWithTokenEstimate(ps *protocol.Session, session *model.Session, threshold int64) {
	if session == nil || threshold <= 0 {
		return
	}
	estimate := session.ComputeTokenEstimate(threshold)
	if estimate == nil {
		return
	}
	ps.CompressionThreshold = &estimate.CompressionThreshold
	ps.CompressionProgress = &estimate.CompressionProgress
	ps.RoundsUntilCompression = &estimate.RoundsUntilCompression
	ps.EstimatedNextRoundTokens = &estimate.EstimatedNextRound
}
