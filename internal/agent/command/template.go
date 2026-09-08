package command

import (
	"bytes"
	"text/template"
)

// TemplateConfig declares a simple command that only contributes prompts.
// Templates use Go's text/template syntax with the following variables:
//
//   - {{.Args}} — the free-form text after the prefix
//   - {{.SessionID}} — the session's UUID
type TemplateConfig struct {
	Name            string // e.g., "persona"
	Prefix          string // e.g., "/persona"
	TriggerScope    Scope  // ScopeOneShot or ScopeSession
	TriggerTemplate string // Rendered on the turn the command is triggered.
	SustainTemplate string // Optional. Rendered on subsequent turns while active. Empty = no sustain contribution.
	Role            string // "system" or "user"
}

// Template constructs a Command from a TemplateConfig.
// The returned command implements Command + Scoped + PromptContributor.
func Template(cfg TemplateConfig) Command {
	return &templateCommand{cfg: cfg}
}

type templateCommand struct {
	cfg TemplateConfig
}

func (t *templateCommand) Name() string   { return t.cfg.Name }
func (t *templateCommand) Prefix() string { return t.cfg.Prefix }
func (t *templateCommand) Scope() Scope   { return t.cfg.TriggerScope }

func (t *templateCommand) TriggerPrompt(ctx Context, args string) (*PromptContribution, error) {
	content, err := renderTemplate(t.cfg.Name+"_trigger", t.cfg.TriggerTemplate, templateData(ctx, args))
	if err != nil {
		return nil, err
	}
	return &PromptContribution{Role: t.cfg.Role, Content: content}, nil
}

func (t *templateCommand) SustainPrompt(ctx Context, args string) (*PromptContribution, error) {
	if t.cfg.SustainTemplate == "" {
		return nil, nil
	}
	content, err := renderTemplate(t.cfg.Name+"_sustain", t.cfg.SustainTemplate, templateData(ctx, args))
	if err != nil {
		return nil, err
	}
	return &PromptContribution{Role: t.cfg.Role, Content: content}, nil
}

type templateDataT struct {
	Args      string
	SessionID string
}

func templateData(ctx Context, args string) templateDataT {
	return templateDataT{
		Args:      args,
		SessionID: ctx.SessionID.String(),
	}
}

func renderTemplate(name, text string, data any) (string, error) {
	t, err := template.New(name).Parse(text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
