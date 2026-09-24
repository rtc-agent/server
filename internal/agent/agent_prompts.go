// agent_prompts.go — Default agent prompt.
//
// This provides the default agent prompt when the frontend doesn't supply one.
// Frontend-generated agent prompts should follow this structure and style.
//
// The agent prompt defines the agent's identity, workflow, and capabilities.
// It is always present (with a default fallback) and injected before the
// system prompt which contains universal behavioral rules.
package agent

import (
	_ "embed"
)

//go:embed prompts/agent/default.md
var defaultAgentPrompt string

// GetDefaultAgentPrompt returns the default agent prompt.
// This is used when session.AgentPrompt is empty.
func GetDefaultAgentPrompt() string {
	return defaultAgentPrompt
}
