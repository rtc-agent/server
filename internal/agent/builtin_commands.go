package agent

import (
	_ "embed"

	"github.com/rtc-agent/server/internal/agent/command"
)

//go:embed prompts/persona-trigger.md.tmpl
var personaTriggerTmpl string

//go:embed prompts/persona-sustain.md.tmpl
var personaSustainTmpl string

// builtinCommands returns the set of simple slash commands registered at
// startup alongside GoalWorkflow. These are pure-prompt commands that need
// no state or tools, and are expressed declaratively via command.Template.
//
// To add a new simple command, append another Template entry here.
func builtinCommands() []command.Command {
	return []command.Command{
		// /persona — switch the agent's role/persona for the session.
		// Example: /persona senior-backend-engineer
		command.Template(command.TemplateConfig{
			Name:            "persona",
			Prefix:          "/persona",
			TriggerScope:    command.ScopeSession,
			Role:            "system",
			TriggerTemplate: personaTriggerTmpl,
			SustainTemplate: personaSustainTmpl,
		}),
	}
}
