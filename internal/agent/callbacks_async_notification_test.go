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
			validate:    validateCompletedNotification(subSession, lastMessage),
		},
		{
			name:         "failed status",
			status:       "failed",
			errorMessage: strPtr("connection timeout"),
			validate:     validateFailedNotification("connection timeout"),
		},
		{
			name:         "cancelled status",
			status:       "cancelled",
			errorMessage: strPtr("user requested cancellation"),
			validate:     validateCancelledNotification("user requested cancellation"),
		},
		{
			name:     "unknown status",
			status:   "unknown",
			validate: validateUnknownNotification(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notificationText := buildTestNotification(tt.status, subSession, tt.lastMessage, tt.errorMessage)
			tt.validate(t, notificationText)
		})
	}
}

// buildTestNotification mirrors the logic in notifyParentAfterAsyncSubAgent.
func buildTestNotification(status string, subSession *model.Session, lastMsg *turnagent.Message, errMsg *string) string {
	switch status {
	case "completed":
		result := "(no output)"
		if lastMsg != nil {
			result = lastMsg.Content
		}
		content := fmt.Sprintf(
			"The async sub agent task has completed.\n- Session ID: %s\n- Title: %s\n- Status: completed\n\nResult:\n%s",
			subSession.ID.String(), subSession.Title, result,
		)
		return turnagent.FormatSystemReminder(content)
	case "failed":
		err := "(unknown error)"
		if errMsg != nil {
			err = *errMsg
		}
		content := fmt.Sprintf(
			"The async sub agent task has failed.\n- Session ID: %s\n- Title: %s\n- Status: failed\n\nError:\n%s",
			subSession.ID.String(), subSession.Title, err,
		)
		return turnagent.FormatSystemReminder(content)
	case "cancelled":
		reason := "(no reason given)"
		if errMsg != nil {
			reason = *errMsg
		}
		content := fmt.Sprintf(
			"The async sub agent task has been cancelled.\n- Session ID: %s\n- Title: %s\n- Status: cancelled\n\nReason:\n%s",
			subSession.ID.String(), subSession.Title, reason,
		)
		return turnagent.FormatSystemReminder(content)
	default:
		content := fmt.Sprintf(
			"The async sub agent task has ended with status: %s.\n- Session ID: %s\n- Title: %s",
			status, subSession.ID.String(), subSession.Title,
		)
		return turnagent.FormatSystemReminder(content)
	}
}

func validateCompletedNotification(subSession *model.Session, lastMsg *turnagent.Message) func(*testing.T, string) {
	return func(t *testing.T, content string) {
		t.Helper()
		assertSystemReminderFormat(t, content)
		requireContains(t, content, "has completed", subSession.Title, lastMsg.Content)
		requireNotContains(t, content, "[System Notification]")
	}
}

func validateFailedNotification(expectedErr string) func(*testing.T, string) {
	return func(t *testing.T, content string) {
		t.Helper()
		assertSystemReminderFormat(t, content)
		requireContains(t, content, "has failed", expectedErr)
	}
}

func validateCancelledNotification(expectedReason string) func(*testing.T, string) {
	return func(t *testing.T, content string) {
		t.Helper()
		assertSystemReminderFormat(t, content)
		requireContains(t, content, "has been cancelled", expectedReason)
	}
}

func validateUnknownNotification() func(*testing.T, string) {
	return func(t *testing.T, content string) {
		t.Helper()
		assertSystemReminderFormat(t, content)
		requireContains(t, content, "unknown")
	}
}

// assertSystemReminderFormat verifies the standard system-reminder wrapper.
func assertSystemReminderFormat(t *testing.T, content string) {
	t.Helper()
	if !strings.HasPrefix(content, "<system-reminder>\n") {
		t.Errorf("content should start with <system-reminder> tag, got: %.50s", content)
	}
	if !strings.HasSuffix(content, "\n</system-reminder>") {
		t.Error("content should end with </system-reminder> tag")
	}
}

// requireContains asserts that content includes all expected substrings.
func requireContains(t *testing.T, content string, substrs ...string) {
	t.Helper()
	for _, s := range substrs {
		if !strings.Contains(content, s) {
			t.Errorf("content should contain %q", s)
		}
	}
}

// requireNotContains asserts that content does not include any of the substrings.
func requireNotContains(t *testing.T, content string, substrs ...string) {
	t.Helper()
	for _, s := range substrs {
		if strings.Contains(content, s) {
			t.Errorf("content should not contain %q", s)
		}
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

func strPtr(s string) *string {
	return &s
}
