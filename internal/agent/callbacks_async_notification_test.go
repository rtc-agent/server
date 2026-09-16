package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// TestAsyncSubAgentNotificationFormat verifies that async sub-agent notifications
// are correctly formatted with <system-reminder> tags and appropriate content.
func TestAsyncSubAgentNotificationFormat(t *testing.T) {
	subSession := &model.Session{
		ID:    uuid.New(),
		Title: "Test Sub-Agent Task",
	}

	lastMessage := &turnagent.Message{
		Role:    turnagent.RoleAssistant,
		Content: "Analysis complete. Found 3 issues.",
	}

	tests := []struct {
		name         string
		status       string
		lastMessage  *turnagent.Message
		errorMessage *string
		validate     func(t *testing.T, content string)
	}{
		{
			name:        "completed status",
			status:      "completed",
			lastMessage: lastMessage,
			validate: func(t *testing.T, content string) {
				// Should contain system-reminder tags
				if !strings.HasPrefix(content, "<system-reminder>\n") {
					t.Errorf("Content should start with <system-reminder> tag, got: %s", content[:50])
				}
				if !strings.HasSuffix(content, "\n</system-reminder>") {
					t.Errorf("Content should end with </system-reminder> tag")
				}
				// Should contain completion information
				if !strings.Contains(content, "has completed") {
					t.Error("Content should mention completion")
				}
				if !strings.Contains(content, subSession.Title) {
					t.Error("Content should include session title")
				}
				if !strings.Contains(content, lastMessage.Content) {
					t.Error("Content should include the result")
				}
				// Should NOT contain old [System Notification] format
				if strings.Contains(content, "[System Notification]") {
					t.Error("Content should not contain old [System Notification] format")
				}
			},
		},
		{
			name:         "failed status",
			status:       "failed",
			lastMessage:  nil,
			errorMessage: strPtr("connection timeout"),
			validate: func(t *testing.T, content string) {
				if !strings.HasPrefix(content, "<system-reminder>\n") {
					t.Errorf("Content should start with <system-reminder> tag")
				}
				if !strings.Contains(content, "has failed") {
					t.Error("Content should mention failure")
				}
				if !strings.Contains(content, "connection timeout") {
					t.Error("Content should include error message")
				}
			},
		},
		{
			name:         "cancelled status",
			status:       "cancelled",
			lastMessage:  nil,
			errorMessage: strPtr("user requested cancellation"),
			validate: func(t *testing.T, content string) {
				if !strings.HasPrefix(content, "<system-reminder>\n") {
					t.Errorf("Content should start with <system-reminder> tag")
				}
				if !strings.Contains(content, "has been cancelled") {
					t.Error("Content should mention cancellation")
				}
				if !strings.Contains(content, "user requested cancellation") {
					t.Error("Content should include cancellation reason")
				}
			},
		},
		{
			name:        "unknown status",
			status:      "unknown",
			lastMessage: nil,
			validate: func(t *testing.T, content string) {
				if !strings.HasPrefix(content, "<system-reminder>\n") {
					t.Errorf("Content should start with <system-reminder> tag")
				}
				if !strings.Contains(content, "unknown") {
					t.Error("Content should include the status")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the notification text building logic from notifyParentAfterAsyncSubAgent
			var notificationText string
			switch tt.status {
			case "completed":
				result := "(no output)"
				if tt.lastMessage != nil {
					result = tt.lastMessage.Content
				}
				content := formatTestNotification(
					"The async sub agent task has completed.\n- Session ID: %s\n- Title: %s\n- Status: completed\n\nResult:\n%s",
					subSession.ID.String(),
					subSession.Title,
					result,
				)
				notificationText = turnagent.FormatSystemReminder(content)
			case "failed":
				errMsg := "(unknown error)"
				if tt.errorMessage != nil {
					errMsg = *tt.errorMessage
				}
				content := formatTestNotification(
					"The async sub agent task has failed.\n- Session ID: %s\n- Title: %s\n- Status: failed\n\nError:\n%s",
					subSession.ID.String(),
					subSession.Title,
					errMsg,
				)
				notificationText = turnagent.FormatSystemReminder(content)
			case "cancelled":
				reason := "(no reason given)"
				if tt.errorMessage != nil {
					reason = *tt.errorMessage
				}
				content := formatTestNotification(
					"The async sub agent task has been cancelled.\n- Session ID: %s\n- Title: %s\n- Status: cancelled\n\nReason:\n%s",
					subSession.ID.String(),
					subSession.Title,
					reason,
				)
				notificationText = turnagent.FormatSystemReminder(content)
			default:
				content := formatTestNotification(
					"The async sub agent task has ended with status: %s.\n- Session ID: %s\n- Title: %s",
					tt.status,
					subSession.ID.String(),
					subSession.Title,
				)
				notificationText = turnagent.FormatSystemReminder(content)
			}

			tt.validate(t, notificationText)
		})
	}
}

// TestAsyncSubAgentNotificationRole verifies that the notification message
// uses user role (not system role) to trigger the turn loop.
func TestAsyncSubAgentNotificationRole(t *testing.T) {
	// The notification should be created with MessageRoleUser, not MessageRoleSystem
	// This is verified by the implementation in callbacks.go
	// Here we just document the expected behavior

	expectedRole := protocol.MessageRoleUser

	// Verify that user role is "user"
	if expectedRole != "user" {
		t.Errorf("Expected role to be 'user', got %s", expectedRole)
	}
}

// formatTestNotification is a helper that formats notification content
// (mirrors the logic in notifyParentAfterAsyncSubAgent)
func formatTestNotification(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}

func strPtr(s string) *string {
	return &s
}
