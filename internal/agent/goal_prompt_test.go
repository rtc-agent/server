package agent

import (
	"testing"

	"github.com/rtc-agent/server/internal/model"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

func TestBuildGoalManagementPrompt(t *testing.T) {
	goal := &model.Goal{
		Condition:      "All 42 tests pass",
		CompletedTurns: 2,
		MaxTurns:       50,
	}

	prompt := buildGoalManagementPrompt(goal)

	// Check that prompt contains key information
	if prompt == "" {
		t.Error("prompt should not be empty")
	}

	// Check that condition is included
	if !contains(prompt, "All 42 tests pass") {
		t.Error("prompt should contain goal condition")
	}

	// Check that progress is included
	if !contains(prompt, "Turn 2/50") {
		t.Error("prompt should contain progress information")
	}

	// Check that tool names are mentioned
	if !contains(prompt, "complete_goal") {
		t.Error("prompt should mention complete_goal tool")
	}
	if !contains(prompt, "cancel_goal") {
		t.Error("prompt should mention cancel_goal tool")
	}
}

func TestInjectGoalCreationPromptIfNeeded(t *testing.T) {
	tests := []struct {
		name           string
		messages       []*turnagent.Message
		expectedLength int
		shouldInject   bool
	}{
		{
			name:           "empty messages",
			messages:       []*turnagent.Message{},
			expectedLength: 0,
			shouldInject:   false,
		},
		{
			name: "no /goal prefix",
			messages: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hello"},
			},
			expectedLength: 1,
			shouldInject:   false,
		},
		{
			name: "/goal exact match",
			messages: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "/goal"},
			},
			expectedLength: 2,
			shouldInject:   true,
		},
		{
			name: "/goal with content",
			messages: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "/goal fix all tests"},
			},
			expectedLength: 2,
			shouldInject:   true,
		},
		{
			name: "/goalify should not match",
			messages: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "/goalify something"},
			},
			expectedLength: 1,
			shouldInject:   false,
		},
		{
			name: "/goal in middle of message should not match",
			messages: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "please /goal this"},
			},
			expectedLength: 1,
			shouldInject:   false,
		},
		{
			name: "last user message is /goal",
			messages: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "I'm ready"},
				{Role: turnagent.RoleUser, Content: "/goal fix bugs"},
			},
			expectedLength: 3,
			shouldInject:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := injectGoalCreationPromptIfNeeded(tt.messages)
			if len(result) != tt.expectedLength {
				t.Errorf("expected length %d, got %d", tt.expectedLength, len(result))
			}
			if tt.shouldInject {
				// Check that last message is the goal creation prompt
				lastMsg := result[len(result)-1]
				if lastMsg.Role != turnagent.RoleSystem {
					t.Error("injected message should be role=system")
				}
				if !contains(lastMsg.Content, "Goal Creation") {
					t.Error("injected message should contain 'Goal Creation'")
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
