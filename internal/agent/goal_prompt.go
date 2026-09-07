package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// goalCreationPrompt is injected as a system message when a /goal command is detected.
// This prompt instructs the Agent on how to handle goal creation workflow.
//
// Phase 2 of /goal command implementation.
const goalCreationPrompt = `# Goal Creation

The user wants to set a goal. Their original input is in the conversation history.

## Your Role

Help the user transform a vague intent into a concrete, measurable completion condition following SMART principles.

## Workflow

### 1. Investigate
- Use available tools to understand the current state (run tests, check linters, inspect files)
- Understand what the user is trying to achieve

### 2. Refine
- Transform the vague intent into a concrete, measurable completion condition
- The condition must be SMART:
  - **S**pecific: Clear about what needs to be done
  - **M**easurable: How to determine completion
  - **A**chievable: Within current capabilities
  - **R**elevant: Aligned with user's actual need
  - **T**ime-bound: Bounded by ` + "`max_turns`" + `

### 3. Propose
- Present the refined condition to the user and ask for confirmation
- Example: "I understand you want to fix all tests. Currently 7 of 42 tests are failing. Suggested goal: All 42 tests pass. Agree?"

### 4. Create
- After user confirms, **call ` + "`create_goal`" + ` tool** with the final condition as the ` + "`condition`" + ` parameter

## Constraints

- You **MUST** follow the workflow in order — do not skip investigation
- You **MUST** call ` + "`create_goal`" + ` after user confirmation — this is mandatory
- You **MUST NOT** call ` + "`create_goal`" + ` if there is already an active goal
- You **MUST NOT** decide the condition for the user — confirmation is required

## Example

User input: ` + "`/goal get all tests passing`" + `

Your response:
1. Run tests, find 7 of 42 failing
2. Propose: "Suggested goal: All 42 tests pass. Agree?"
3. After user agrees, call ` + "`create_goal(condition: \"All 42 tests pass\")`" + `
`

// injectGoalCreationPromptIfNeeded checks if the last user message starts with "/goal"
// and, if so, appends a system message with the goal creation prompt.
// This is the Phase 2 implementation of the /goal command.
//
// The injected system message is NOT persisted to the database — it only exists
// in the message list sent to the LLM for this turn. On subsequent turns, the
// detection runs again on the fresh message history.
//
// Prefix matching rules:
//   - "/goal" alone: matches
//   - "/goal <content>": matches (space required after /goal)
//   - "/goalify" or other prefixes: does NOT match
func injectGoalCreationPromptIfNeeded(messages []*turnagent.Message) []*turnagent.Message {
	if len(messages) == 0 {
		return messages
	}

	// Find the last user message by iterating backwards.
	var lastUserContent string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == turnagent.RoleUser {
			lastUserContent = messages[i].Content
			break
		}
	}

	// Check if the last user message starts with "/goal".
	// Must be exactly "/goal" or "/goal " (with space after).
	if !strings.HasPrefix(lastUserContent, "/goal") {
		return messages
	}
	// If there's more content after "/goal", it must be followed by a space.
	if len(lastUserContent) > len("/goal") && lastUserContent[len("/goal")] != ' ' {
		return messages
	}

	// Append the goal creation system message.
	goalMsg := &turnagent.Message{
		Role:    turnagent.RoleSystem,
		Content: goalCreationPrompt,
	}
	return append(messages, goalMsg)
}

// injectGoalManagementPromptIfNeeded checks if there is an active goal for the session
// and, if so, appends a user-role message with the goal management prompt.
// This is the Phase 3 implementation of the goal execution loop.
//
// The injected message is NOT persisted to the database — it only exists
// in the message list sent to the LLM for this turn.
func (h *helpers) injectGoalManagementPromptIfNeeded(ctx context.Context, sessionID string, messages []*turnagent.Message) []*turnagent.Message {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return messages
	}

	// Check if GoalRepo is available
	if h.deps.GoalRepo == nil {
		return messages
	}

	// Query active goal
	goal, err := h.deps.GoalRepo.FindActive(ctx, sid)
	if err != nil || goal == nil {
		return messages
	}

	// Build and append the goal management prompt as role=user
	prompt := buildGoalManagementPrompt(goal)
	goalMsg := &turnagent.Message{
		Role:    turnagent.RoleUser,
		Content: prompt,
	}
	return append(messages, goalMsg)
}

// buildGoalManagementPrompt builds the goal management prompt for runtime injection.
func buildGoalManagementPrompt(goal *model.Goal) string {
	return fmt.Sprintf(`# Goal Management

You are working on a persistent goal:
- Condition: %s
- Progress: Turn %d/%d

## Your responsibilities

1. Review current progress by checking message history and running verification tools (tests, linters, etc.)
2. If the condition is fully satisfied, call `+"`complete_goal`"+` tool with a clear reason
3. If the condition cannot be achieved, call `+"`cancel_goal`"+` tool with a clear reason
4. Otherwise, continue working toward the goal

## Reporting

- Briefly report current status to the user
- If continuing, state your next action

## Constraints

- You MUST explicitly evaluate goal status each turn
- You MUST call `+"`complete_goal`"+` or `+"`cancel_goal`"+` when appropriate — this is mandatory
- Do not skip verification steps before calling `+"`complete_goal`"+``,
		goal.Condition,
		goal.CompletedTurns, goal.MaxTurns,
	)
}
