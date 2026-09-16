package agent

import (
	"testing"
	"time"

	"github.com/rtc-agent/server/internal/usecase/primitives"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

func TestIntDeref(t *testing.T) {
	tests := []struct {
		name     string
		input    *int
		expected int
	}{
		{"nil returns 0", nil, 0},
		{"non-nil returns value", intPtr(42), 42},
		{"zero value", intPtr(0), 0},
		{"negative value", intPtr(-1), -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := intDeref(tt.input)
			if result != tt.expected {
				t.Errorf("intDeref() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

func TestBuildMessagesFromSummaryItems(t *testing.T) {
	now := time.Now()

	t.Run("empty items", func(t *testing.T) {
		msgs := buildMessagesFromSummaryItems(nil, now)
		if len(msgs) != 0 {
			t.Errorf("expected 0 messages, got %d", len(msgs))
		}
	})

	t.Run("single item marked as boundary", func(t *testing.T) {
		items := []primitives.SummaryItem{
			{Role: "system", Content: "summary text"},
		}
		msgs := buildMessagesFromSummaryItems(items, now)
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %d", len(msgs))
		}
		if msgs[0].Role != "system" {
			t.Errorf("role = %q, want %q", msgs[0].Role, "system")
		}
		if msgs[0].Content != "summary text" {
			t.Errorf("content = %q, want %q", msgs[0].Content, "summary text")
		}
		if msgs[0].CreatedAt != now {
			t.Error("CreatedAt not set correctly")
		}
		// Single item is both first and last, so it should have the boundary marker.
		if msgs[0].Extra == nil {
			t.Fatal("Extra should be set for the last item (boundary marker)")
		}
		boundary, ok := msgs[0].Extra[turnagent.ExtraKeySummaryBoundary].(bool)
		if !ok || !boundary {
			t.Error("last item should have ExtraKeySummaryBoundary = true")
		}
	})

	t.Run("multiple items only last has boundary", func(t *testing.T) {
		items := []primitives.SummaryItem{
			{Role: "system", Content: "first"},
			{Role: "user", Content: "second"},
			{Role: "assistant", Content: "third"},
		}
		msgs := buildMessagesFromSummaryItems(items, now)
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}

		// First message should NOT have boundary marker.
		if msgs[0].Extra != nil {
			if _, ok := msgs[0].Extra[turnagent.ExtraKeySummaryBoundary]; ok {
				t.Error("first item should NOT have boundary marker")
			}
		}

		// Middle message should NOT have boundary marker.
		if msgs[1].Extra != nil {
			if _, ok := msgs[1].Extra[turnagent.ExtraKeySummaryBoundary]; ok {
				t.Error("middle item should NOT have boundary marker")
			}
		}

		// Last message SHOULD have boundary marker.
		if msgs[2].Extra == nil {
			t.Fatal("last item should have Extra set")
		}
		boundary, ok := msgs[2].Extra[turnagent.ExtraKeySummaryBoundary].(bool)
		if !ok || !boundary {
			t.Error("last item should have ExtraKeySummaryBoundary = true")
		}
	})
}
