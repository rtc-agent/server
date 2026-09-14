// workflow_prompts.go — Goal and Loop workflow prompt templates.
//
// These templates are injected into the LLM context by GoalWorkflow and
// LoopWorkflow to guide the agent through structured goal/loop management.
//
// Two categories:
//   - Static prompts (goal-creation.md, loop-creation.md): used as-is.
//   - Dynamic templates (goal-management.md.tmpl, loop-management.md.tmpl):
//     rendered with runtime data via mustRenderTemplate.
//
// Naming convention:
//   - Static variables: <name>Prompt (e.g. goalCreationPrompt)
//   - Template variables: <name>Tmpl (e.g. goalManagementTmpl)
//   - Builders: build<Name>Prompt(args...) string
package agent

import (
	_ "embed"

	"github.com/rtc-agent/server/internal/model"
)

//go:embed prompts/goal-creation.md
var goalCreationPrompt string

//go:embed prompts/goal-management.md.tmpl
var goalManagementTmpl string

//go:embed prompts/loop-creation.md
var loopCreationPrompt string

//go:embed prompts/loop-management.md.tmpl
var loopManagementTmpl string

// buildGoalManagementPrompt builds the goal management prompt for runtime injection.
// Consumed by GoalWorkflow.SustainPrompt (see goal_workflow.go).
func buildGoalManagementPrompt(goal *model.Goal) string {
	return mustRenderTemplate("goal_management", goalManagementTmpl, goal)
}

// buildLoopManagementPrompt builds the loop management prompt for runtime injection.
// Consumed by LoopWorkflow.SustainPrompt (see loop_workflow.go).
func buildLoopManagementPrompt(loop *model.Loop) string {
	return mustRenderTemplate("loop_management", loopManagementTmpl, loop)
}
