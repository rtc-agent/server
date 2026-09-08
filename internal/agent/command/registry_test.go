package command

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// --- minimal test doubles ---

type fakeCmd struct {
	name, prefix string
	scope        Scope
	triggerContent, sustainContent string
	triggerErr, sustainErr         error
	triggerCalls, sustainCalls     int
	turnCalls                      int
}

func (f *fakeCmd) Name() string   { return f.name }
func (f *fakeCmd) Prefix() string { return f.prefix }
func (f *fakeCmd) Scope() Scope   { return f.scope }

func (f *fakeCmd) TriggerPrompt(ctx Context, args string) (*PromptContribution, error) {
	f.triggerCalls++
	if f.triggerErr != nil {
		return nil, f.triggerErr
	}
	if f.triggerContent == "" {
		return nil, nil
	}
	return &PromptContribution{Role: "system", Content: f.triggerContent + "|" + args}, nil
}

func (f *fakeCmd) SustainPrompt(ctx Context, args string) (*PromptContribution, error) {
	f.sustainCalls++
	if f.sustainErr != nil {
		return nil, f.sustainErr
	}
	if f.sustainContent == "" {
		return nil, nil
	}
	return &PromptContribution{Role: "user", Content: f.sustainContent + "|" + args}, nil
}

type hookCmd struct {
	fakeCmd
	hookErr error
}

func (h *hookCmd) OnTurnComplete(ctx Context) error {
	h.turnCalls++
	return h.hookErr
}

func newCtx(sid string) Context {
	id, _ := uuid.Parse(sid)
	return Context{Context: context.Background(), SessionID: id}
}

// --- tests ---

func TestDefaultDetect(t *testing.T) {
	cases := []struct {
		prefix, msg string
		wantMatch   bool
		wantArgs    string
	}{
		{"/goal", "/goal", true, ""},
		{"/goal", "/goal fix tests", true, "fix tests"},
		{"/goal", "/goal  fix tests", true, "fix tests"}, // extra space trimmed
		{"/goal", "/goalify", false, ""},                  // no false prefix match
		{"/goal", "/goals", false, ""},
		{"/goal", "goal", false, ""},
		{"/goal", "", false, ""},
	}
	for _, c := range cases {
		args, matched := defaultDetect(c.prefix, c.msg)
		if matched != c.wantMatch || args != c.wantArgs {
			t.Errorf("defaultDetect(%q, %q) = (%q, %v); want (%q, %v)",
				c.prefix, c.msg, args, matched, c.wantArgs, c.wantMatch)
		}
	}
}

func TestDetectAndInject_OneShotTrigger(t *testing.T) {
	r := NewCommandRegistry()
	cmd := &fakeCmd{name: "ping", prefix: "/ping", scope: ScopeOneShot, triggerContent: "pong"}
	r.Register(cmd)

	ctx := newCtx("11111111-1111-1111-1111-111111111111")
	contribs, err := r.DetectAndInject(ctx, "/ping hello")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(contribs) != 1 {
		t.Fatalf("want 1 contribution, got %d", len(contribs))
	}
	if contribs[0].CommandName != "ping" {
		t.Errorf("command name = %q; want ping", contribs[0].CommandName)
	}
	if contribs[0].Contribution.Content != "pong|hello" {
		t.Errorf("content = %q; want pong|hello", contribs[0].Contribution.Content)
	}
	if cmd.triggerCalls != 1 {
		t.Errorf("triggerCalls = %d; want 1", cmd.triggerCalls)
	}

	// OnTurnComplete should drop one-shot commands.
	r.OnTurnComplete(ctx)
	if got := r.Active(ctx.SessionID); len(got) != 0 {
		t.Errorf("after OnTurnComplete, active = %v; want empty", got)
	}

	// Second turn: command no longer active, no contributions.
	contribs2, _ := r.DetectAndInject(ctx, "anything")
	if len(contribs2) != 0 {
		t.Errorf("second turn contributions = %d; want 0", len(contribs2))
	}
}

func TestDetectAndInject_SessionScope(t *testing.T) {
	r := NewCommandRegistry()
	cmd := &fakeCmd{
		name: "persona", prefix: "/persona", scope: ScopeSession,
		triggerContent: "activated", sustainContent: "still-active",
	}
	r.Register(cmd)
	ctx := newCtx("22222222-2222-2222-2222-222222222222")

	// Turn 1: trigger.
	c1, _ := r.DetectAndInject(ctx, "/persona expert")
	if len(c1) != 1 || c1[0].Contribution.Content != "activated|expert" {
		t.Fatalf("turn 1: %+v", c1)
	}

	// Turn 1 completes: session-scoped → survives.
	r.OnTurnComplete(ctx)
	if got := r.Active(ctx.SessionID); len(got) != 1 || got[0] != "persona" {
		t.Fatalf("after turn 1 complete, active = %v", got)
	}

	// Turn 2: no trigger, sustain.
	c2, _ := r.DetectAndInject(ctx, "continue working")
	if len(c2) != 1 || c2[0].Contribution.Content != "still-active|expert" {
		t.Fatalf("turn 2: %+v", c2)
	}
}

func TestDetectAndInject_RetriggersUpdatesArgs(t *testing.T) {
	r := NewCommandRegistry()
	cmd := &fakeCmd{
		name: "persona", prefix: "/persona", scope: ScopeSession,
		triggerContent: "T", sustainContent: "S",
	}
	r.Register(cmd)
	ctx := newCtx("33333333-3333-3333-3333-333333333333")

	if _, err := r.DetectAndInject(ctx, "/persona expert"); err != nil {
		t.Fatalf("DetectAndInject: %v", err)
	}
	r.OnTurnComplete(ctx)

	// Re-trigger with new args.
	c, _ := r.DetectAndInject(ctx, "/persona novice")
	if len(c) != 1 || c[0].Contribution.Content != "T|novice" {
		t.Fatalf("retrigger: %+v", c)
	}
	if cmd.triggerCalls != 2 {
		t.Errorf("triggerCalls = %d; want 2", cmd.triggerCalls)
	}

	// Sustain uses updated args.
	r.OnTurnComplete(ctx)
	c2, _ := r.DetectAndInject(ctx, "continue")
	if len(c2) != 1 || c2[0].Contribution.Content != "S|novice" {
		t.Fatalf("sustain after retrigger: %+v", c2)
	}
}

func TestDetectAndInject_FirstMatchWins(t *testing.T) {
	r := NewCommandRegistry()
	a := &fakeCmd{name: "a", prefix: "/x", triggerContent: "A"}
	b := &fakeCmd{name: "b", prefix: "/x", triggerContent: "B"}
	r.Register(a)
	r.Register(b)

	ctx := newCtx("44444444-4444-4444-4444-444444444444")
	c, _ := r.DetectAndInject(ctx, "/x hi")
	if len(c) != 1 || c[0].CommandName != "a" {
		t.Fatalf("first match should win: %+v", c)
	}
}

func TestDetectAndInject_DegradationOnError(t *testing.T) {
	r := NewCommandRegistry()
	bad := &fakeCmd{name: "bad", prefix: "/bad", triggerErr: errors.New("boom")}
	good := &fakeCmd{name: "good", prefix: "/good", triggerContent: "OK"}
	r.Register(bad)
	r.Register(good)

	ctx := newCtx("55555555-5555-5555-5555-555555555555")
	c, err := r.DetectAndInject(ctx, "/good hi")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Only "good" should contribute; "bad" has no match.
	if len(c) != 1 || c[0].CommandName != "good" {
		t.Fatalf("got %+v", c)
	}

	// Register a matching bad command: error should degrade, not block others.
	r2 := NewCommandRegistry()
	bad2 := &fakeCmd{name: "bad2", prefix: "/y", triggerErr: errors.New("boom")}
	good2 := &fakeCmd{name: "good2", prefix: "/z", triggerContent: "OK"}
	r2.Register(bad2)
	r2.Register(good2)

	c2, _ := r2.DetectAndInject(ctx, "/z hi")
	if len(c2) != 1 || c2[0].CommandName != "good2" {
		t.Fatalf("degradation: %+v", c2)
	}
}

func TestDetectAndInject_CustomDetector(t *testing.T) {
	r := NewCommandRegistry()
	cmd := &customDetectCmd{name: "ctx", triggerContent: "injected"}
	r.Register(cmd)

	ctx := newCtx("66666666-6666-6666-6666-666666666666")

	// Doesn't match without "add" subcommand.
	c1, _ := r.DetectAndInject(ctx, "/ctx")
	if len(c1) != 0 {
		t.Fatalf("no match expected: %+v", c1)
	}

	// Matches with "add".
	c2, _ := r.DetectAndInject(ctx, "/ctx add some fact")
	if len(c2) != 1 || c2[0].Contribution.Content != "injected" {
		t.Fatalf("match expected: %+v", c2)
	}
}

type customDetectCmd struct {
	fakeCmd
}

func (c *customDetectCmd) Detect(msg string) (string, bool) {
	const prefix = "/ctx add "
	if len(msg) > len(prefix)-1 && msg[:len(prefix)-1] == "/ctx add" {
		// Exact prefix match; return rest as args.
		if len(msg) == len(prefix)-1 {
			return "", true
		}
		if msg[len(prefix)-1] == ' ' {
			return msg[len(prefix):], true
		}
	}
	return "", false
}

func (c *customDetectCmd) Name() string   { return c.name }
func (c *customDetectCmd) Prefix() string { return "/ctx" }
func (c *customDetectCmd) TriggerPrompt(ctx Context, args string) (*PromptContribution, error) {
	c.triggerCalls++
	return &PromptContribution{Role: "user", Content: c.triggerContent}, nil
}
func (c *customDetectCmd) SustainPrompt(ctx Context, args string) (*PromptContribution, error) {
	return nil, nil
}

func TestDeactivate(t *testing.T) {
	r := NewCommandRegistry()
	cmd := &fakeCmd{name: "persona", prefix: "/persona", scope: ScopeSession, triggerContent: "x"}
	r.Register(cmd)
	ctx := newCtx("77777777-7777-7777-7777-777777777777")
	if _, err := r.DetectAndInject(ctx, "/persona expert"); err != nil {
		t.Fatalf("DetectAndInject: %v", err)
	}
	if len(r.Active(ctx.SessionID)) != 1 {
		t.Fatal("expected 1 active")
	}
	r.Deactivate(ctx.SessionID, "persona")
	if len(r.Active(ctx.SessionID)) != 0 {
		t.Fatal("expected 0 active after deactivate")
	}
}

func TestOnTurnComplete_CollectsHookErrors(t *testing.T) {
	r := NewCommandRegistry()
	h := &hookCmd{
		fakeCmd: fakeCmd{name: "h", prefix: "/h", scope: ScopeSession, triggerContent: "x"},
		hookErr: errors.New("hook-fail"),
	}
	r.Register(h)
	ctx := newCtx("88888888-8888-8888-8888-888888888888")
	if _, err := r.DetectAndInject(ctx, "/h"); err != nil {
		t.Fatalf("DetectAndInject: %v", err)
	}
	errs := r.OnTurnComplete(ctx)
	if len(errs) != 1 || errs[0].Error() != "hook-fail" {
		t.Fatalf("expected hook error, got %v", errs)
	}
	if h.turnCalls != 1 {
		t.Errorf("turnCalls = %d; want 1", h.turnCalls)
	}
}

func TestTemplate_Simple(t *testing.T) {
	cmd := Template(TemplateConfig{
		Name:            "persona",
		Prefix:          "/persona",
		TriggerScope:    ScopeSession,
		TriggerTemplate: "You are {{.Args}}.",
		SustainTemplate: "Reminder: {{.Args}}",
		Role:            "system",
	})
	if cmd.Name() != "persona" || cmd.Prefix() != "/persona" {
		t.Fatalf("identity: %v %v", cmd.Name(), cmd.Prefix())
	}
	if s, ok := cmd.(Scoped); !ok || s.Scope() != ScopeSession {
		t.Fatalf("scope: want ScopeSession")
	}

	ctx := newCtx("99999999-9999-9999-9999-999999999999")
	pc, ok := cmd.(PromptContributor)
	if !ok {
		t.Fatal("template should implement PromptContributor")
	}

	c, err := pc.TriggerPrompt(ctx, "an expert")
	if err != nil {
		t.Fatalf("trigger err: %v", err)
	}
	if c.Content != "You are an expert." || c.Role != "system" {
		t.Errorf("trigger = %+v", c)
	}

	c2, err := pc.SustainPrompt(ctx, "an expert")
	if err != nil {
		t.Fatalf("sustain err: %v", err)
	}
	if c2.Content != "Reminder: an expert" {
		t.Errorf("sustain = %+v", c2)
	}
}
