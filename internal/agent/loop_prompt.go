package agent

import (
	"fmt"

	"github.com/rtc-agent/server/internal/model"
)

// loopCreationPrompt is injected as a system message when a /loop command is detected.
// This prompt instructs the Agent on how to handle loop creation workflow.
//
// Consumed by LoopWorkflow.TriggerPrompt (see loop_workflow.go).
const loopCreationPrompt = `# Loop Creation

The user wants to set up a recurring loop. Their original input is in the conversation history.

## Your Role

Help the user transform a vague intent into a concrete, recurring task with clear parameters.

## Workflow

### 1. Investigate
- Understand what the user wants to do repeatedly
- Determine the appropriate interval and scope

### 2. Refine
- Transform the vague intent into a concrete, actionable prompt
- The prompt must be:
  - **Clear**: Unambiguous about what to do each turn
  - **Bounded**: Has a natural stopping condition or max turns
  - **Interval-appropriate**: The interval makes sense for the task

### 3. Propose
- Present the refined plan to the user and ask for confirmation
- Example: "I understand you want to monitor the deployment status every 60 seconds. Suggested loop: Check deployment health every 60s, max 10 turns. Agree?"

### 4. Create
- After user confirms, **call ` + "`create_loop`" + ` tool** with the final parameters

## Constraints

- You **MUST** follow the workflow in order — do not skip investigation
- You **MUST** call ` + "`create_loop`" + ` after user confirmation — this is mandatory
- You **MUST NOT** call ` + "`create_loop`" + ` if there is already an active loop
- You **MUST NOT** call ` + "`create_loop`" + ` if there is an active goal (they are mutually exclusive)
- You **MUST NOT** decide the parameters for the user — confirmation is required

## Example

User input: ` + "`/loop check deployment status every minute`" + `

Your response:
1. Propose: "I'll set up a loop to check deployment health every 60 seconds, max 10 turns. Agree?"
2. After user agrees, call ` + "`create_loop(prompt: \"Check deployment health and report status\", interval_seconds: 60)`" + `
`

// buildLoopManagementPrompt builds the loop management prompt for runtime injection.
// Consumed by LoopWorkflow.SustainPrompt (see loop_workflow.go).
func buildLoopManagementPrompt(loop *model.Loop) string {
	return fmt.Sprintf(`# Loop Management

You are executing a recurring loop:
- Prompt: %s
- Progress: Turn %d/%d
- Interval: %d seconds

## Your responsibilities

1. Execute the loop prompt for this turn
2. Report results to the user
3. If the loop's objective is fully achieved, call `+"`complete_loop`"+` tool with a clear reason
4. If the loop cannot continue or user wants to stop, call `+"`cancel_loop`"+` tool with a clear reason
5. Otherwise, continue executing the loop prompt

## Reporting

- Briefly report current turn results to the user
- State what will happen next (loop will continue after interval)

## Constraints

- You MUST execute the loop prompt each turn
- You MUST call `+"`complete_loop`"+` or `+"`cancel_loop`"+` when appropriate — this is mandatory
- Do not skip execution steps before calling `+"`complete_loop`"+``,
		loop.Prompt,
		loop.CompletedTurns, loop.MaxTurns,
		loop.IntervalSeconds,
	)
}
