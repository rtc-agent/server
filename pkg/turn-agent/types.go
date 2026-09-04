package turnagent

// WorkKind identifies the kind of work item carried in an rtc-queue Work's Data
// field. Two kinds exist; both flow through the same Agent.Process entry point.
type WorkKind string

const (
	// WorkKindSubmit starts a new turn. The publisher (API layer) creates a
	// Work with this kind when a user sends a message or triggers any action
	// that should spawn a fresh agent turn.
	//
	// turn-agent is responsible for creating the turn: it calls
	// Config.CreateTurn to allocate a turnID, then proceeds with the turn's
	// lifecycle.
	WorkKindSubmit WorkKind = "submit"

	// WorkKindResume continues an interrupted turn from the eino checkpoint.
	// The publisher creates a Work with this kind when an external event
	// (e.g., an RTC result submission) signals that the interrupted turn
	// may proceed.
	//
	// turn-agent locates the existing turn via Config.LookupTurn, then
	// continues the turn's lifecycle.
	//
	// rtc-queue priority ordering guarantees Resume items are always claimed
	// before Submit items for the same session, so a resumed turn always
	// sees its own checkpoint intact.
	WorkKindResume WorkKind = "resume"
)

// WorkPayload is the JSON-decoded content of an rtc-queue Work item's Data
// field. It is the only data flow from the publisher (API layer) into the
// agent.
//
// The payload is intentionally minimal: it carries only the session identity
// and the work kind. Turn identity (turnID) is created/looked up by the
// agent via Config callbacks; message identity and other per-work details
// are loaded from the application's store by data callbacks (LoadMessages,
// etc.). This keeps the publisher free of turn-lifecycle concerns.
//
// Large data (RTC results, conversation history, etc.) lives in the
// application's database or in the eino checkpoint, not in the work payload.
type WorkPayload struct {
	// Kind identifies whether this work starts a fresh turn or resumes one.
	Kind WorkKind `json:"kind"`

	// SessionID identifies the session this work belongs to. It is used to
	// derive the eino CheckpointID, to scope data-callback invocations, and
	// to look up / create the turn via Config callbacks.
	SessionID string `json:"session_id"`
}
