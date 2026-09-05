package agent

import (
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// =============================================================================
// RTC tool types
// =============================================================================
//
// The RTC (Remote Tool Call) tools enable the LLM to request operations on the
// client's virtual filesystem (ls, read, write, grep, find, script). Each tool
// call creates a Message + RTC record in the DB, publishes updates to the
// frontend, and pauses the turn via tool.StatefulInterrupt. The client executes
// the operation in the browser and submits the result, which triggers a resume
// work item that re-enters the tool to retrieve the result.
//
// Mapping from old code: the old internal/worker/tools.go defined identical
// tool types (LSTool, ReadTool, etc.) that delegated to a BaseRTC. The new
// code defines them here in the agent package with the same delegation pattern.
// Tool extraction to a shared package (TODO 4) is deferred; the current
// implementation is fully functional within the agent package.

// rtcToolBase is the base for all RTC tools in the new agent package.
// It holds the session, helpers (for DB access), and the current turn ID.
type rtcToolBase struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// rtcInterruptInfo is passed to StatefulInterrupt as the info parameter.
// It identifies the interrupt type and carries the RTC ID so the interrupt
// handler can route to the correct pub/sub channel.
// Must be gob-serializable (struct with exported fields).
//
// Mapping from old code: identical to rtcInterruptInfo in
// internal/worker/tools.rtc.go.
type rtcInterruptInfo struct {
	Type      string // "rtc"
	ToolName  string
	RtcID     string
	MessageID string
	Args      string // JSON-encoded tool arguments
}

// rtcInterruptState is passed to StatefulInterrupt as the state parameter.
// When the tool resumes from a checkpoint, it reads this state to find the
// RTC result.
// Must be gob-serializable (struct with exported fields).
//
// Mapping from old code: identical to rtcInterruptState in
// internal/worker/tools.rtc.go.
type rtcInterruptState struct {
	RtcID      string
	ToolCallID string
	MessageID  string
	ToolName   string
}
