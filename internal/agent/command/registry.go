package command

import (
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/google/uuid"
)

// CommandRegistry manages command registration, per-session activation, and
// orchestration of prompt injection, tool collection, and turn callbacks.
//
// Thread-safe: all public methods may be called concurrently from different
// sessions.
type CommandRegistry struct {
	mu         sync.RWMutex
	registered []Command
	activated  map[uuid.UUID][]*activatedEntry // sessionID → entries
}

type activatedEntry struct {
	cmd               Command
	args              string
	triggeredThisTurn bool
	activatedAt       time.Time
}

// NewCommandRegistry creates an empty registry.
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{
		activated: make(map[uuid.UUID][]*activatedEntry),
	}
}

// Register adds a command. Typically called at startup. Order is preserved
// and determines detection priority and prompt/tool concatenation order.
func (r *CommandRegistry) Register(cmd Command) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registered = append(r.registered, cmd)
}

// Registered returns all registered commands in registration order.
func (r *CommandRegistry) Registered() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Command, len(r.registered))
	copy(out, r.registered)
	return out
}

// DetectAndInject scans lastUserMsg against registered commands, updates
// activation state, and returns prompt contributions for this turn.
//
// Detection order = registration order. A single user message can trigger
// at most one command (the first matching prefix wins).
//
// Contributions are returned in registration order. Newly-triggered
// commands receive TriggerPrompt; already-active commands receive
// SustainPrompt. Each contribution is paired with the contributing
// command's name for tagging/observability.
//
// The caller is responsible for converting PromptContributions to messages
// and appending them to the LLM context.
func (r *CommandRegistry) DetectAndInject(ctx Context, lastUserMsg string) ([]NamedContribution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Phase 1: detect the (first) matching command, if any.
	var triggeredCmd Command
	var triggeredArgs string
	for _, cmd := range r.registered {
		args, matched := detect(cmd, lastUserMsg)
		if matched {
			triggeredCmd = cmd
			triggeredArgs = args
			break
		}
	}

	// Phase 2: update activation records.
	entries := r.activated[ctx.SessionID]

	// Reset every entry's triggeredThisTurn; we'll re-mark the one that
	// actually matched this turn (if any).
	for _, e := range entries {
		e.triggeredThisTurn = false
	}

	if triggeredCmd != nil {
		found := false
		for _, e := range entries {
			if e.cmd.Name() == triggeredCmd.Name() {
				e.args = triggeredArgs
				e.triggeredThisTurn = true
				found = true
				break
			}
		}
		if !found {
			entries = append(entries, &activatedEntry{
				cmd:               triggeredCmd,
				args:              triggeredArgs,
				triggeredThisTurn: true,
				activatedAt:       time.Now(),
			})
		}
		r.activated[ctx.SessionID] = entries
	}

	// Phase 3: collect contributions in registration order.
	// Build an index from name → entry for O(1) lookup.
	entryByName := make(map[string]*activatedEntry, len(entries))
	for _, e := range entries {
		entryByName[e.cmd.Name()] = e
	}

	var contributions []NamedContribution
	for _, cmd := range r.registered {
		e, ok := entryByName[cmd.Name()]
		if !ok {
			continue
		}
		pc, ok := cmd.(PromptContributor)
		if !ok {
			continue
		}

		// Populate args in context for this command's call.
		cmdCtx := ctx
		cmdCtx.Args = e.args

		var contrib *PromptContribution
		var err error
		if e.triggeredThisTurn {
			contrib, err = pc.TriggerPrompt(cmdCtx, e.args)
		} else {
			contrib, err = pc.SustainPrompt(cmdCtx, e.args)
		}
		if err != nil {
			// Degrade: skip this command's contribution.
			continue
		}
		if contrib != nil {
			contributions = append(contributions, NamedContribution{
				CommandName:  cmd.Name(),
				Contribution: contrib,
			})
		}
	}
	return contributions, nil
}

// NamedContribution pairs a prompt contribution with the command that
// produced it. The CommandName is useful for wrapping/tagging the prompt
// so the LLM can distinguish sources.
type NamedContribution struct {
	CommandName  string
	Contribution *PromptContribution
}

// CollectTools returns all tools from currently-active commands, in
// registration order.
func (r *CommandRegistry) CollectTools(ctx Context) []tool.BaseTool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := r.activated[ctx.SessionID]

	entryByName := make(map[string]*activatedEntry, len(entries))
	for _, e := range entries {
		entryByName[e.cmd.Name()] = e
	}

	var tools []tool.BaseTool
	for _, cmd := range r.registered {
		e, ok := entryByName[cmd.Name()]
		if !ok {
			continue
		}
		tp, ok := cmd.(ToolProvider)
		if !ok {
			continue
		}
		cmdCtx := ctx
		cmdCtx.Args = e.args
		tools = append(tools, tp.Tools(cmdCtx)...)
	}
	return tools
}

// OnTurnComplete invokes TurnHook.OnTurnComplete for all active commands in
// registration order, then deactivates one-shot commands. Errors from hooks
// are collected and returned; they do not interrupt other hooks.
func (r *CommandRegistry) OnTurnComplete(ctx Context) []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.activated[ctx.SessionID]
	var errs []error
	var surviving []*activatedEntry
	for _, e := range entries {
		if h, ok := e.cmd.(TurnHook); ok {
			cmdCtx := ctx
			cmdCtx.Args = e.args
			if err := h.OnTurnComplete(cmdCtx); err != nil {
				errs = append(errs, err)
			}
		}
		if scopeOf(e.cmd) == ScopeSession {
			e.triggeredThisTurn = false
			surviving = append(surviving, e)
		}
		// OneShot commands are dropped here.
	}
	if len(surviving) == 0 {
		delete(r.activated, ctx.SessionID)
	} else {
		r.activated[ctx.SessionID] = surviving
	}
	return errs
}

// Deactivate explicitly removes a command from a session's active list.
func (r *CommandRegistry) Deactivate(sessionID uuid.UUID, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.activated[sessionID]
	var surviving []*activatedEntry
	for _, e := range entries {
		if e.cmd.Name() != name {
			surviving = append(surviving, e)
		}
	}
	if len(surviving) == 0 {
		delete(r.activated, sessionID)
	} else {
		r.activated[sessionID] = surviving
	}
}

// Active returns the names of commands currently active for a session, in
// activation order.
func (r *CommandRegistry) Active(sessionID uuid.UUID) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := r.activated[sessionID]
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.cmd.Name())
	}
	return out
}

// --- helpers ---

func detect(cmd Command, msg string) (args string, matched bool) {
	if d, ok := cmd.(Detector); ok {
		return d.Detect(msg)
	}
	return defaultDetect(cmd.Prefix(), msg)
}

// defaultDetect matches "<Prefix>" exactly or "<Prefix> <anything>". The
// comparison is case-sensitive. A prefix like "/goal" does not match
// "/goalify".
func defaultDetect(prefix, msg string) (string, bool) {
	if msg == prefix {
		return "", true
	}
	if strings.HasPrefix(msg, prefix+" ") {
		rest := msg[len(prefix)+1:]
		return strings.TrimLeft(rest, " "), true
	}
	return "", false
}

func scopeOf(cmd Command) Scope {
	if s, ok := cmd.(Scoped); ok {
		return s.Scope()
	}
	return ScopeOneShot
}
