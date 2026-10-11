// Package converter provides an adapter layer between RPC handlers and the model package.
//
// RPC handlers import this package instead of internal/model directly, maintaining
// proper layer separation (Handler -> Usecase -> Repo). This package re-exports
// domain types and conversion functions needed by handlers for protocol adaptation.
package converter

import (
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ============================================================
// Domain type aliases (re-exported from model)
// ============================================================

// Session represents a conversation session.
type Session = model.Session

// Message represents a single message within a session.
type Message = model.Message

// Turn represents a processing turn within a session.
type Turn = model.Turn

// Rtc represents a Real-Time Computation unit (tool invocation).
type Rtc = model.Rtc

// StringArray is a JSON-serializable string slice stored as JSONB.
type StringArray = model.StringArray

// JSONBString is a string stored as JSONB (with JSON quoting).
type JSONBString = model.JSONBString

// ScriptExecution records script execution details.
type ScriptExecution = model.ScriptExecution

// ============================================================
// Utility functions (re-exported from model)
// ============================================================

// DerefStr dereferences a *string; returns empty string for nil.
func DerefStr(s *string) string { return model.DerefStr(s) }

// ============================================================
// Protocol conversion functions (re-exported from model)
// ============================================================
//
// TODO: Move protocol conversion to usecase layer.
// Currently using converter package as an adapter to maintain proper layer separation
// (Handler -> Usecase -> Repo). Future: usecase methods should return protocol types
// directly, eliminating the need for this adapter layer. This would require creating
// corresponding protocol return types for each usecase method.

// ToProtocolSession converts a domain Session to a protocol Session.
func ToProtocolSession(m *Session) protocol.Session { return model.ToProtocolSession(m) }

// ToProtocolTurn converts a domain Turn to a protocol Turn.
func ToProtocolTurn(t *Turn) protocol.Turn { return model.ToProtocolTurn(t) }

// ToProtocolMessage converts a domain Message to a protocol Message.
func ToProtocolMessage(m *Message) protocol.Message { return model.ToProtocolMessage(m) }

// ToProtocolRtc converts a domain Rtc to a protocol Rtc.
func ToProtocolRtc(r *Rtc) protocol.Rtc { return model.ToProtocolRtc(r) }
