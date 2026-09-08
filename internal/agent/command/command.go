// Package command provides the slash-command framework for the RTC agent.
//
// A Command is a named entity that can contribute prompts and tools to the
// LLM call, and receive turn lifecycle callbacks. Commands are identified by
// a slash prefix (e.g., "/goal", "/persona") detected on the last user
// message during loadMessages.
//
// All capability interfaces (Scoped, Detector, PromptContributor,
// ToolProvider, TurnHook) are optional — a command implements only what it
// needs. The CommandRegistry orchestrates detection, prompt collection,
// tool collection, and turn callbacks uniformly.
package command

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
	"github.com/google/uuid"
)

// Command is the minimal identity of a slash command.
//
// Name is a short slug ("goal", "persona"); Prefix is the slash-trigger
// ("/goal", "/persona"). Name is used for logging/observability; Prefix is
// used for detection.
type Command interface {
	Name() string
	Prefix() string
}

// Scope controls how long a command stays active after being triggered.
type Scope int

const (
	// ScopeOneShot: command is active only on the turn it was triggered.
	// Automatically deactivated after OnTurnComplete.
	ScopeOneShot Scope = iota

	// ScopeSession: command stays active for the entire session until
	// explicitly deactivated or the session ends.
	ScopeSession
)

// Scoped declares a command's activation scope.
// Commands that don't implement Scoped default to ScopeOneShot.
type Scoped interface {
	Scope() Scope
}

// Detector customizes how the command matches the last user message.
// Commands that don't implement Detector use a default prefix-equals matcher:
// the message must be exactly "<Prefix>" or start with "<Prefix> ".
type Detector interface {
	Detect(lastUserMsg string) (args string, matched bool)
}

// PromptContribution is a prompt fragment a command contributes to the LLM
// context. Role must be "system" or "user".
type PromptContribution struct {
	Role    string
	Content string
}

// PromptContributor contributes prompts to the LLM call.
//
// TriggerPrompt is called on the turn the command is freshly triggered (the
// last user message matched its prefix). The args are everything after the
// prefix.
//
// SustainPrompt is called on subsequent turns while the command remains
// active (session-scoped). The args reflect the most recent trigger (or
// re-trigger). Return nil to skip contributing this turn.
type PromptContributor interface {
	TriggerPrompt(ctx Context, args string) (*PromptContribution, error)
	SustainPrompt(ctx Context, args string) (*PromptContribution, error)
}

// ToolProvider contributes tools to the LLM call.
// Called during createTools. Commands that don't need tools can omit this.
type ToolProvider interface {
	Tools(ctx Context) []tool.BaseTool
}

// TurnHook receives callbacks at turn boundaries.
// Called during completeTurn after the LLM has finished.
type TurnHook interface {
	OnTurnComplete(ctx Context) error
}

// Context carries per-call data to command methods.
//
// The embedded context.Context carries cancellation/deadline. SessionID is
// always set. TurnID is zero when not applicable (e.g., during loadMessages
// before the turn is fully created).
//
// Args is populated by the registry before invoking PromptContributor
// methods; it reflects the command's current activation args.
type Context struct {
	context.Context
	SessionID uuid.UUID
	TurnID    uuid.UUID
	Args      string
}
