package turnagent

// EventKind identifies the kind of event delivered to PublishEventFunc.
//
// The upper application switches on Kind to dispatch business logic (persist a
// chunk, finalize a streaming message row, deliver a complete message, log an
// error). It does NOT consume eino's stream objects or inspect eino's event
// structure — the pkg handles all eino flow internally and surfaces flattened,
// application-friendly events.
type EventKind string

const (
	// EventKindStreamChunk is one chunk from a streaming response.
	//
	// Delivered once per schema.Message received from the underlying stream.
	// Content carries the markdown fragment; ReasoningContent carries the
	// thinking fragment; either or both may be non-empty. FinishReason is
	// populated on the final chunk of the stream (e.g., "stop", "tool_calls")
	// and empty on all other chunks.
	//
	// The upper application is expected to track its own "current streaming
	// message row" state (e.g., one row for markdown, one for thinking) and
	// append each chunk accordingly. The pkg does not maintain this state —
	// it is inherently tied to the application's DB schema.
	EventKindStreamChunk EventKind = "stream_chunk"

	// EventKindStreamEnd signals that a streaming response has finished (the
	// underlying stream returned io.EOF).
	//
	// The upper application finalizes its streaming message rows here
	// (e.g., marks streaming_status = "completed"). The stream's Role and
	// ToolName fields are still populated on this event so the application
	// can identify which stream ended.
	EventKindStreamEnd EventKind = "stream_end"

	// EventKindMessage carries a complete, non-streaming message.
	//
	// Used when the model returned its output in a single shot (no streaming).
	// The Message field is populated.
	EventKindMessage EventKind = "message"

	// EventKindError reports an event-level error during agent execution.
	//
	// Distinct from turn-level failures (which go through FailTurn): this is
	// an error attached to a single event in the agent's output stream. The
	// upper application typically logs it and continues; the turn may still
	// complete successfully.
	EventKindError EventKind = "error"
)

// Event represents one callback from the agent's execution, delivered to
// PublishEventFunc.
//
// Exactly one of the "content" fields is meaningful per Kind:
//
//   - EventKindStreamChunk: Content / ReasoningContent / FinishReason
//   - EventKindStreamEnd:   (no content fields; Role / ToolName identify the stream)
//   - EventKindMessage:     Message
//   - EventKindError:       Err
//
// Metadata fields (AgentName, Role, ToolName) are populated on all events
// where they are meaningful.
type Event struct {
	// Kind identifies which content fields are populated.
	Kind EventKind

	// AgentName is the name of the agent that produced this event. Useful for
	// multi-agent scenarios; empty for single-agent setups.
	AgentName string

	// Role is the role of the message source: "assistant" or "tool". Empty for
	// error events.
	Role string

	// ToolName identifies the tool that produced this event. Populated when
	// Role == "tool"; empty otherwise.
	ToolName string

	// ----- EventKindStreamChunk fields -----

	// Content is the markdown fragment in this chunk. May be empty if this
	// chunk carries only reasoning content.
	Content string

	// ReasoningContent is the thinking fragment in this chunk. May be empty if
	// this chunk carries only markdown content.
	ReasoningContent string

	// FinishReason is populated on the final chunk of a stream (e.g., "stop",
	// "tool_calls"). Empty on all other chunks and on non-streaming events.
	FinishReason string

	// ----- EventKindMessage field -----

	// Message is the complete, non-streaming message. Populated only for
	// EventKindMessage.
	Message *Message

	// ----- EventKindError field -----

	// Err is the event-level error. Populated only for EventKindError.
	Err error
}
