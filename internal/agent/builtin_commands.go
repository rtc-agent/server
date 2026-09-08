package agent

import (
	"github.com/rtc-agent/server/internal/agent/command"
)

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
			Name:         "persona",
			Prefix:       "/persona",
			TriggerScope: command.ScopeSession,
			Role:         "system",
			TriggerTemplate: `# Persona Active

You are now operating under the following persona: **{{.Args}}**

Adopt this role fully: use its vocabulary, priorities, and typical reasoning patterns. When uncertain about how the persona would act, prefer the interpretation most consistent with an experienced practitioner of that role.

This persona remains active for the entire session. The user may change it with another /persona command, or revert to default behavior with /persona default.`,
			SustainTemplate: `(Reminder: you are currently operating as **{{.Args}}**. Stay in character.)`,
		}),
	}
}
