package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── ScopeType tests ───

func TestIsValidScopeType(t *testing.T) {
	tests := []struct {
		name  string
		scope ScopeType
		want  bool
	}{
		{"session is valid", ScopeSession, true},
		{"user is valid", ScopeUser, true},
		{"global is valid", ScopeGlobal, true},
		{"empty is invalid", ScopeType(""), false},
		{"random is invalid", ScopeType("team"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsValidScopeType(tt.scope))
		})
	}
}

// ─── MemoryType tests ───

func TestIsValidMemoryType(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		want bool
	}{
		{"decision", "decision", true},
		{"context", "context", true},
		{"progress", "progress", true},
		{"issue", "issue", true},
		{"learnings", "learnings", true},
		{"user", "user", true},
		{"feedback", "feedback", true},
		{"project", "project", true},
		{"reference", "reference", true},
		{"empty", "", false},
		{"random", "random", false},
		{"case sensitive", "Decision", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsValidMemoryType(tt.typ))
		})
	}
}

// ─── Memory.Validate tests ───

func TestMemory_Validate(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		memory  Memory
		wantErr string
	}{
		{
			name: "valid session memory",
			memory: Memory{
				Scope:     ScopeSession,
				ScopeID:   uuid.New(),
				Type:      "decision",
				Title:     "Use PostgreSQL",
				Content:   "We chose PostgreSQL for its JSONB support.",
				Timestamp: now,
			},
			wantErr: "",
		},
		{
			name: "valid user memory",
			memory: Memory{
				Scope:     ScopeUser,
				ScopeID:   uuid.New(),
				Type:      "feedback",
				Title:     "Prefer terse responses",
				Content:   "User prefers short, direct answers.",
				Timestamp: now,
			},
			wantErr: "",
		},
		{
			name: "valid global memory with zero ScopeID",
			memory: Memory{
				Scope:     ScopeGlobal,
				ScopeID:   uuid.Nil,
				Type:      "reference",
				Title:     "API Docs",
				Content:   "Link to external API docs.",
				Timestamp: now,
			},
			wantErr: "",
		},
		{
			name: "invalid scope",
			memory: Memory{
				Scope:     ScopeType("team"),
				ScopeID:   uuid.New(),
				Type:      "decision",
				Title:     "Test",
				Content:   "Test content.",
				Timestamp: now,
			},
			wantErr: "invalid scope type",
		},
		{
			name: "invalid type",
			memory: Memory{
				Scope:     ScopeSession,
				ScopeID:   uuid.New(),
				Type:      "invalid_type",
				Title:     "Test",
				Content:   "Test content.",
				Timestamp: now,
			},
			wantErr: "invalid memory type",
		},
		{
			name: "empty title",
			memory: Memory{
				Scope:     ScopeSession,
				ScopeID:   uuid.New(),
				Type:      "decision",
				Title:     "",
				Content:   "Test content.",
				Timestamp: now,
			},
			wantErr: "required field missing",
		},
		{
			name: "empty content",
			memory: Memory{
				Scope:     ScopeSession,
				ScopeID:   uuid.New(),
				Type:      "decision",
				Title:     "Test",
				Content:   "",
				Timestamp: now,
			},
			wantErr: "required field missing",
		},
		{
			name: "zero timestamp",
			memory: Memory{
				Scope:     ScopeSession,
				ScopeID:   uuid.New(),
				Type:      "decision",
				Title:     "Test",
				Content:   "Test content.",
				Timestamp: time.Time{},
			},
			wantErr: "required field missing",
		},
		{
			name: "session scope without ScopeID",
			memory: Memory{
				Scope:     ScopeSession,
				ScopeID:   uuid.Nil,
				Type:      "decision",
				Title:     "Test",
				Content:   "Test content.",
				Timestamp: now,
			},
			wantErr: "scopeId is required for session scope",
		},
		{
			name: "user scope without ScopeID",
			memory: Memory{
				Scope:     ScopeUser,
				ScopeID:   uuid.Nil,
				Type:      "feedback",
				Title:     "Test",
				Content:   "Test content.",
				Timestamp: now,
			},
			wantErr: "scopeId is required for user scope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.memory.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

// ─── MemoryLink tests ───

func TestIsValidRelation(t *testing.T) {
	tests := []struct {
		name     string
		relation string
		want     bool
	}{
		{"related", "related", true},
		{"depends_on", "depends_on", true},
		{"supersedes", "supersedes", true},
		{"derives_from", "derives_from", true},
		{"empty", "", false},
		{"random", "random", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsValidRelation(tt.relation))
		})
	}
}

func TestMemoryLink_Validate(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()

	tests := []struct {
		name    string
		link    MemoryLink
		wantErr string
	}{
		{
			name:    "valid link",
			link:    MemoryLink{FromID: id1, ToID: id2, Relation: "related"},
			wantErr: "",
		},
		{
			name:    "nil fromId",
			link:    MemoryLink{FromID: uuid.Nil, ToID: id2, Relation: "related"},
			wantErr: "fromId is required",
		},
		{
			name:    "nil toId",
			link:    MemoryLink{FromID: id1, ToID: uuid.Nil, Relation: "related"},
			wantErr: "toId is required",
		},
		{
			name:    "same fromId and toId",
			link:    MemoryLink{FromID: id1, ToID: id1, Relation: "related"},
			wantErr: "fromId and toId must be different",
		},
		{
			name:    "invalid relation",
			link:    MemoryLink{FromID: id1, ToID: id2, Relation: "invalid"},
			wantErr: "invalid relation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.link.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

// ─── TableName tests ───

func TestMemory_TableName(t *testing.T) {
	assert.Equal(t, "memories", Memory{}.TableName())
}

func TestMemoryLink_TableName(t *testing.T) {
	assert.Equal(t, "memory_links", MemoryLink{}.TableName())
}

// ─── Formatter tests ───

func newTestMemory(memType, title, content string) *Memory {
	return &Memory{
		ID:        uuid.New(),
		Scope:     ScopeSession,
		ScopeID:   uuid.New(),
		Type:      memType,
		Title:     title,
		Content:   content,
		Timestamp: time.Now(),
		CreatedAt: time.Now(),
	}
}

func TestFormatter_FormatForInjection_Empty(t *testing.T) {
	f := NewFormatter()
	result := f.FormatForInjection(nil, "en")
	assert.Empty(t, result)

	result = f.FormatForInjection([]*Memory{}, "en")
	assert.Empty(t, result)
}

func TestFormatter_FormatForInjection_Basic(t *testing.T) {
	f := NewFormatter()
	memories := []*Memory{
		newTestMemory("context", "Working on auth", "Implementing JWT authentication"),
		newTestMemory("decision", "Use RS256", "Chose RS256 for token signing"),
	}

	result := f.FormatForInjection(memories, "en")

	assert.Contains(t, result, "<system-reminder>")
	assert.Contains(t, result, "</system-reminder>")
	assert.Contains(t, result, "**[context]** Working on auth: Implementing JWT authentication")
	assert.Contains(t, result, "**[decision]** Use RS256: Chose RS256 for token signing")
}

func TestFormatter_FormatForInjection_Truncation(t *testing.T) {
	f := NewFormatter()
	longContent := strings.Repeat("a", 300)
	memories := []*Memory{
		newTestMemory("context", "Long content", longContent),
	}

	result := f.FormatForInjection(memories, "en")

	// Content should be truncated to 200 chars + "..."
	assert.Contains(t, result, strings.Repeat("a", 200)+"...")
	assert.NotContains(t, result, strings.Repeat("a", 201))
}

func TestFormatter_FormatForInjection_TypeOrder(t *testing.T) {
	f := NewFormatter()
	// Insert in reverse type order to verify output follows typeOrder
	memories := []*Memory{
		newTestMemory("learnings", "Learned caching", "Redis is fast"),
		newTestMemory("decision", "Use Redis", "For caching layer"),
		newTestMemory("context", "Building cache", "Setting up Redis"),
	}

	result := f.FormatForInjection(memories, "en")

	// context should appear before decision, decision before learnings
	ctxIdx := strings.Index(result, "**[context]**")
	decIdx := strings.Index(result, "**[decision]**")
	learnIdx := strings.Index(result, "**[learnings]**")

	assert.Greater(t, ctxIdx, -1)
	assert.Greater(t, decIdx, ctxIdx)
	assert.Greater(t, learnIdx, decIdx)
}

func TestFormatter_FormatForInjection_MaxPerType(t *testing.T) {
	f := NewFormatter()
	// 5 context items, only 3 should appear
	memories := []*Memory{
		newTestMemory("context", "Context 1", "Content 1"),
		newTestMemory("context", "Context 2", "Content 2"),
		newTestMemory("context", "Context 3", "Content 3"),
		newTestMemory("context", "Context 4", "Content 4"),
		newTestMemory("context", "Context 5", "Content 5"),
	}

	result := f.FormatForInjection(memories, "en")

	assert.Contains(t, result, "Context 1")
	assert.Contains(t, result, "Context 2")
	assert.Contains(t, result, "Context 3")
	assert.NotContains(t, result, "Context 4")
	assert.NotContains(t, result, "Context 5")
}

func TestFormatter_FormatForSummary_Empty(t *testing.T) {
	f := NewFormatter()
	result := f.FormatForSummary(nil)
	assert.Empty(t, result)

	result = f.FormatForSummary([]*Memory{})
	assert.Empty(t, result)
}

func TestFormatter_FormatForSummary_Basic(t *testing.T) {
	f := NewFormatter()
	memories := []*Memory{
		newTestMemory("context", "Current task", "Building memory system"),
		newTestMemory("decision", "Use GORM", "For database access"),
		newTestMemory("issue", "Migration failed", "FK constraint issue"),
	}

	result := f.FormatForSummary(memories)

	assert.Contains(t, result, "# Session Memory")
	assert.Contains(t, result, "## Current Context")
	assert.Contains(t, result, "### Current task")
	assert.Contains(t, result, "Building memory system")
	assert.Contains(t, result, "## Decisions")
	assert.Contains(t, result, "### Use GORM")
	assert.Contains(t, result, "## Issues & Solutions")
	assert.Contains(t, result, "### Migration failed")
}

func TestFormatter_FormatForSummary_FullContent(t *testing.T) {
	f := NewFormatter()
	longContent := strings.Repeat("x", 500)
	memories := []*Memory{
		newTestMemory("context", "Long", longContent),
	}

	result := f.FormatForSummary(memories)

	// Summary should NOT truncate content
	assert.Contains(t, result, longContent)
}

func TestFormatter_FormatForExport_Basic(t *testing.T) {
	f := NewFormatter()
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	m := &Memory{
		ID:          uuid.MustParse("a1b2c3d4-0000-0000-0000-000000000000"),
		Scope:       ScopeUser,
		ScopeID:     uuid.New(),
		Type:        "decision",
		Title:       "Use PostgreSQL",
		Description: "Choose PostgreSQL as main database",
		Content:     "We chose PostgreSQL for its JSONB support and full-text search.",
		Tags:        model.StringArray{"database", "architecture"},
		Resource:    "https://www.postgresql.org/",
		Timestamp:   now,
		CreatedAt:   now,
	}

	result := f.FormatForExport(m)

	// Check frontmatter
	assert.Contains(t, result, "---\n")
	assert.Contains(t, result, "type: decision\n")
	assert.Contains(t, result, "title: Use PostgreSQL\n")
	assert.Contains(t, result, "description: Choose PostgreSQL as main database\n")
	assert.Contains(t, result, "tags:\n  - database\n  - architecture\n")
	assert.Contains(t, result, "resource: https://www.postgresql.org/\n")
	assert.Contains(t, result, "generated:\n  by: rtc-agent/1.0\n")
	assert.Contains(t, result, "  at: \"2026-09-14T10:00:00Z\"\n")

	// Check markdown body
	assert.Contains(t, result, "# Use PostgreSQL\n\n")
	assert.Contains(t, result, "We chose PostgreSQL for its JSONB support and full-text search.\n")
}

func TestFormatter_FormatForExport_MinimalFields(t *testing.T) {
	f := NewFormatter()
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	m := &Memory{
		Type:      "context",
		Title:     "Current task",
		Content:   "Building the memory system.",
		Timestamp: now,
		CreatedAt: now,
	}

	result := f.FormatForExport(m)

	// Should have type and title, but no description/tags/resource
	assert.Contains(t, result, "type: context\n")
	assert.Contains(t, result, "title: Current task\n")
	assert.NotContains(t, result, "description:")
	assert.NotContains(t, result, "tags:")
	assert.NotContains(t, result, "resource:")

	// Should still have generated block
	assert.Contains(t, result, "generated:\n")
}

func TestFormatter_FormatForExport_EmptyTags(t *testing.T) {
	f := NewFormatter()
	m := &Memory{
		Type:      "progress",
		Title:     "Done",
		Content:   "Finished.",
		Tags:      model.StringArray{},
		Timestamp: time.Now(),
		CreatedAt: time.Now(),
	}

	result := f.FormatForExport(m)
	assert.NotContains(t, result, "tags:")
}

func TestFormatter_FormatForSummary_AllTypes(t *testing.T) {
	f := NewFormatter()
	memories := []*Memory{
		newTestMemory("context", "Current task", "Building memory system"),
		newTestMemory("progress", "Done", "Finished implementation"),
		newTestMemory("decision", "Use GORM", "For database access"),
		newTestMemory("issue", "Bug found", "Fixed null pointer"),
		newTestMemory("learnings", "Cache helps", "Redis is fast"),
		newTestMemory("user", "Engineer", "Backend developer"),
		newTestMemory("feedback", "Be terse", "Short answers please"),
		newTestMemory("project", "RTC-Agent", "AI assistant project"),
		newTestMemory("reference", "Go docs", "https://go.dev"),
	}

	result := f.FormatForSummary(memories)

	// All 9 types should appear
	assert.Contains(t, result, "## Current Context")
	assert.Contains(t, result, "## Progress")
	assert.Contains(t, result, "## Decisions")
	assert.Contains(t, result, "## Issues & Solutions")
	assert.Contains(t, result, "## Learnings")
	assert.Contains(t, result, "## About User")
	assert.Contains(t, result, "## Feedback & Preferences")
	assert.Contains(t, result, "## Project Info")
	assert.Contains(t, result, "## References")
}

func TestFormatter_FormatForSummary_UnknownType(t *testing.T) {
	f := NewFormatter()
	memories := []*Memory{
		newTestMemory("context", "Known", "Known content"),
		newTestMemory("future_type", "Unknown", "Unknown content"),
	}

	result := f.FormatForSummary(memories)

	assert.Contains(t, result, "## Current Context")
	assert.Contains(t, result, "## Other")
	assert.Contains(t, result, "### Unknown")
}

func TestFormatter_FormatForExport_YAMLSpecialChars(t *testing.T) {
	f := NewFormatter()
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	m := &Memory{
		Type:        "decision",
		Title:       "Use DB: PostgreSQL",
		Description: "A \"great\" choice #1",
		Content:     "Content body",
		Timestamp:   now,
		CreatedAt:   now,
	}

	result := f.FormatForExport(m)

	// Title and Description should be YAML-quoted to handle special chars
	assert.Contains(t, result, `title: "Use DB: PostgreSQL"`)
	assert.Contains(t, result, `description: "A \"great\" choice #1"`)
}

func TestFormatter_FormatForExport_YAMLPlainValue(t *testing.T) {
	f := NewFormatter()
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	m := &Memory{
		Type:      "context",
		Title:     "Simple title without special chars",
		Content:   "Content body",
		Timestamp: now,
		CreatedAt: now,
	}

	result := f.FormatForExport(m)

	// Plain title should NOT be quoted
	assert.Contains(t, result, "title: Simple title without special chars\n")
}
