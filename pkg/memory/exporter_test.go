package memory

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── slugify tests ───

func TestSlugify(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"spaces", "Hello World", "hello-world"},
		{"title", "Use PostgreSQL", "use-postgresql"},
		{"multiple spaces", "  Multiple   Spaces  ", "multiple-spaces"},
		{"special chars", "Special!@#Characters", "special-characters"},
		{"already slug", "Already-Slugged", "already-slugged"},
		{"uppercase", "UPPERCASE", "uppercase"},
		{"camel case", "CamelCase", "camelcase"},
		{"chinese only", "中文标题", "untitled"},
		{"empty", "", "untitled"},
		{"only special", "!!!", "untitled"},
		{"mixed chinese", "Mix 中文 Test", "mix-test"},
		{"dashes", "a-b-c-d", "a-b-c-d"},
		{"trailing dashes", "trailing---", "trailing"},
		{"leading dashes", "---leading", "leading"},
		{"numbers", "Issue 123 Fix", "issue-123-fix"},
		{"underscore", "hello_world", "hello-world"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, slugify(tt.input))
		})
	}
}

// ─── titleCase tests ───

func TestTitleCase(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "Hello"},
		{"hello world", "Hello World"},
		{"use-postgres", "Use-Postgres"},
		{"already Title", "Already Title"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, titleCase(tt.input))
		})
	}
}

// ─── typeToDirectory tests ───

func TestTypeToDirectory(t *testing.T) {
	tests := []struct {
		typ string
		dir string
	}{
		{"decision", "decisions"},
		{"context", "context"},
		{"progress", "progress"},
		{"issue", "issues"},
		{"learnings", "learnings"},
		{"user", "user"},
		{"feedback", "feedback"},
		{"project", "project"},
		{"reference", "references"},
		{"unknown", "misc"},
		{"", "misc"},
		{"custom_type", "misc"},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			assert.Equal(t, tt.dir, typeToDirectory(tt.typ))
		})
	}
}

// ─── uuidShort tests ───

func TestUuidShort(t *testing.T) {
	id := uuid.MustParse("a1b2c3d4-0000-0000-0000-000000000000")
	assert.Equal(t, "a1b2c3d4", uuidShort(id))
}

// ─── Export tests ───

func TestExportOptions_NoIncludeLinksField(t *testing.T) {
	// Compile-time check: ExportOptions must not have IncludeLinks field (P2-3 fix).
	// This test verifies the struct only has the expected fields by constructing
	// a valid ExportOptions with known fields. If IncludeLinks were added back,
	// this test would still compile, but the reflection check below would catch it.
	opts := ExportOptions{
		Scope:      ScopeUser,
		ScopeID:    uuid.New(),
		Types:      []string{"decision"},
		Tags:       []string{"test"},
		IncludeLog: true,
	}
	assert.Equal(t, ScopeUser, opts.Scope)
	assert.True(t, opts.IncludeLog)

	// Reflection check: ensure no "IncludeLinks" field exists
	val := reflect.ValueOf(opts)
	typ := val.Type()
	for i := 0; i < typ.NumField(); i++ {
		assert.NotEqual(t, "IncludeLinks", typ.Field(i).Name,
			"ExportOptions must not have IncludeLinks field (P2-3 fix: IncludeLinks removed)")
	}
}

func TestExportEmpty(t *testing.T) {
	repo := &mockRepo{memories: []*Memory{}}
	exporter := NewExporter(repo)

	var buf bytes.Buffer
	scopeID := uuid.New()
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// Should have index.md
	var indexContent string
	for name, content := range files {
		if strings.HasSuffix(name, "/index.md") {
			indexContent = content
			break
		}
	}
	require.NotEmpty(t, indexContent, "index.md should exist")
	assert.Contains(t, indexContent, `okf_version: "0.2"`)
	assert.Contains(t, indexContent, "Total memories**: 0")

	// Should NOT have log.md (IncludeLog defaults to false)
	for name := range files {
		if strings.HasSuffix(name, "/log.md") {
			t.Error("should not have log.md when IncludeLog is false")
		}
	}
}

func TestExportWithMemories(t *testing.T) {
	scopeID := uuid.New()
	memories := newSampleMemories(scopeID)
	repo := &mockRepo{memories: memories}
	exporter := NewExporter(repo)

	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// index.md should exist with OKF header
	var indexContent string
	for name, content := range files {
		if strings.HasSuffix(name, "/index.md") {
			indexContent = content
			break
		}
	}
	require.NotEmpty(t, indexContent, "index.md not found")
	assert.True(t, strings.HasPrefix(indexContent, "---\nokf_version: \"0.2\"\n---\n"))

	// Should have 3 concept docs (3 user-scope memories, not the session one)
	conceptCount := 0
	for name := range files {
		if strings.HasSuffix(name, ".md") && !strings.HasSuffix(name, "/index.md") {
			conceptCount++
		}
	}
	assert.Equal(t, 3, conceptCount, "expected 3 concept docs, got files: %v", fileNames(files))

	// Check that concept docs are under correct directories
	for name := range files {
		if strings.HasSuffix(name, ".md") && !strings.HasSuffix(name, "/index.md") {
			assert.True(t,
				strings.Contains(name, "/decisions/") || strings.Contains(name, "/context/"),
				"unexpected directory for file: %s", name)
		}
	}

	// Verify concept doc content has OKF frontmatter
	for name, content := range files {
		if strings.Contains(name, "/decisions/") && strings.HasSuffix(name, ".md") {
			assert.Contains(t, content, "---")
			assert.Contains(t, content, "type: decision")
		}
	}
}

func TestExportWithLog(t *testing.T) {
	scopeID := uuid.New()
	memories := newSampleMemories(scopeID)
	repo := &mockRepo{memories: memories}
	exporter := NewExporter(repo)

	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:      ScopeUser,
		ScopeID:    scopeID,
		IncludeLog: true,
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// Should have log.md
	var logContent string
	for name, content := range files {
		if strings.HasSuffix(name, "/log.md") {
			logContent = content
			break
		}
	}
	require.NotEmpty(t, logContent, "log.md not found when IncludeLog=true")

	// log.md should have OKF header
	assert.True(t, strings.HasPrefix(logContent, "---\nokf_version: \"0.2\"\n---\n"))

	// log.md should contain dates
	assert.Contains(t, logContent, "2026-01-")

	// log.md should contain memory types
	assert.Contains(t, logContent, "**Created**")
	assert.Contains(t, logContent, "](/decisions/")
}

func TestExportWithTypeFilter(t *testing.T) {
	scopeID := uuid.New()
	memories := newSampleMemories(scopeID)
	repo := &mockRepo{memories: memories}
	exporter := NewExporter(repo)

	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
		Types:   []string{"decision"},
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// Should only have decision type docs
	conceptCount := 0
	for name := range files {
		if strings.HasSuffix(name, ".md") && !strings.HasSuffix(name, "/index.md") {
			conceptCount++
			assert.Contains(t, name, "/decisions/", "type filter should only include decisions/, got: %s", name)
		}
	}
	assert.Equal(t, 2, conceptCount, "expected 2 decision docs, got files: %v", fileNames(files))
}

func TestExportWithMultipleTypeFilter(t *testing.T) {
	scopeID := uuid.New()
	memories := newSampleMemories(scopeID)
	repo := &mockRepo{memories: memories}
	exporter := NewExporter(repo)

	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
		Types:   []string{"decision", "context"},
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// Should have 3 docs: 2 decisions + 1 context
	conceptCount := 0
	for name := range files {
		if strings.HasSuffix(name, ".md") && !strings.HasSuffix(name, "/index.md") {
			conceptCount++
		}
	}
	assert.Equal(t, 3, conceptCount, "expected 3 docs (2 decisions + 1 context), got files: %v", fileNames(files))
}

func TestExportLogDateOrdering(t *testing.T) {
	scopeID := uuid.New()
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	ids := newTestUUIDs(3)
	memories := []*Memory{
		{
			ID:        ids[0],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "decision",
			Title:     "Latest Decision",
			Content:   "Content",
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        ids[1],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "context",
			Title:     "Earlier Context",
			Content:   "Content",
			Timestamp: now.Add(-48 * time.Hour),
			CreatedAt: now.Add(-48 * time.Hour),
			UpdatedAt: now.Add(-48 * time.Hour),
		},
		{
			ID:        ids[2],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "progress",
			Title:     "Middle Progress",
			Content:   "Content",
			Timestamp: now.Add(-24 * time.Hour),
			CreatedAt: now.Add(-24 * time.Hour),
			UpdatedAt: now.Add(-24 * time.Hour),
		},
	}

	repo := &mockRepo{memories: memories}
	exporter := NewExporter(repo)

	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:      ScopeUser,
		ScopeID:    scopeID,
		IncludeLog: true,
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	var logContent string
	for name, content := range files {
		if strings.HasSuffix(name, "/log.md") {
			logContent = content
			break
		}
	}
	require.NotEmpty(t, logContent)

	// Dates should be in reverse chronological order (newest first)
	idx1 := strings.Index(logContent, "2026-02-27")
	idx2 := strings.Index(logContent, "2026-02-28")
	idx3 := strings.Index(logContent, "2026-03-01")

	require.NotEqual(t, -1, idx1, "expected 2026-02-27 in log")
	require.NotEqual(t, -1, idx2, "expected 2026-02-28 in log")
	require.NotEqual(t, -1, idx3, "expected 2026-03-01 in log")
	assert.True(t, idx3 < idx2 && idx2 < idx1, "dates should be in reverse chronological order (newest first)")
}

func TestExportBundleName(t *testing.T) {
	repo := &mockRepo{memories: []*Memory{}}
	exporter := NewExporter(repo)

	scopeID := uuid.MustParse("abcdef12-3456-7890-abcd-ef1234567890")
	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// Bundle name should be: user-abcdef12-YYYYMMDD-HHmmss/
	for name := range files {
		assert.True(t, strings.HasPrefix(name, "user-abcdef12-"),
			"expected bundle name prefix 'user-abcdef12-', got: %s", name)
		break
	}
}

func TestExportIndexContent(t *testing.T) {
	scopeID := uuid.MustParse("12345678-0000-0000-0000-000000000000")
	memories := newSampleMemories(scopeID)
	repo := &mockRepo{memories: memories}
	exporter := NewExporter(repo)

	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	var indexContent string
	for name, content := range files {
		if strings.HasSuffix(name, "/index.md") {
			indexContent = content
			break
		}
	}
	require.NotEmpty(t, indexContent)

	// Check OKF header
	assert.True(t, strings.HasPrefix(indexContent, "---\nokf_version: \"0.2\"\n---\n"))

	// Check scope info
	assert.Contains(t, indexContent, "**Scope**: user")
	assert.Contains(t, indexContent, "**Scope ID**: 12345678-")

	// Check total count
	assert.Contains(t, indexContent, "**Total memories**: 3")

	// Check links use OKF bundle-relative paths
	assert.Contains(t, indexContent, "](/decisions/")
}

func TestExportWithUnknownType(t *testing.T) {
	scopeID := uuid.New()
	id := uuid.New()
	repo := &mockRepo{
		memories: []*Memory{
			{
				ID:        id,
				Scope:     ScopeUser,
				ScopeID:   scopeID,
				Type:      "totally_unknown",
				Title:     "Unknown Type Memory",
				Content:   "Content",
				Timestamp: time.Now(),
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
		},
	}

	exporter := NewExporter(repo)
	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// Should be under misc/
	found := false
	for name := range files {
		if strings.Contains(name, "/misc/") {
			found = true
			break
		}
	}
	assert.True(t, found, "unknown type should be under misc/")
}

func TestExportWithTagFilter(t *testing.T) {
	scopeID := uuid.New()
	memories := newSampleMemories(scopeID)
	repo := &mockRepo{memories: memories}
	exporter := NewExporter(repo)

	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
		Tags:    []string{"database"},
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// Only "Use PostgreSQL" has the "database" tag
	conceptCount := 0
	for name := range files {
		if strings.HasSuffix(name, ".md") && !strings.HasSuffix(name, "/index.md") {
			conceptCount++
		}
	}
	assert.Equal(t, 1, conceptCount, "expected 1 doc matching tag 'database', got files: %v", fileNames(files))
}

func TestGenerateIndexFormat(t *testing.T) {
	exporter := NewExporter(&mockRepo{})
	scopeID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	opts := ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
	}
	memories := []*Memory{
		{
			ID:    uuid.MustParse("11111111-0000-0000-0000-000000000000"),
			Type:  "decision",
			Title: "Use Postgres",
		},
	}

	result := exporter.generateIndex(opts, memories)

	assert.Contains(t, result, `okf_version: "0.2"`)
	assert.Contains(t, result, "# User aaaaaaaa Memory Bundle")
	assert.Contains(t, result, "**Scope**: user")
	assert.Contains(t, result, "](/decisions/11111111-use-postgres.md)")
}

func TestGenerateLogFormat(t *testing.T) {
	exporter := NewExporter(&mockRepo{})
	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	memories := []*Memory{
		{
			Type:      "decision",
			Title:     "Use Postgres",
			CreatedAt: now,
		},
		{
			Type:      "context",
			Title:     "Web Project",
			CreatedAt: now.Add(-24 * time.Hour),
		},
	}

	result := exporter.generateLog(memories)

	assert.Contains(t, result, `okf_version: "0.2"`)
	assert.Contains(t, result, "# Memory Log")
	assert.Contains(t, result, "## 2026-01-14")
	assert.Contains(t, result, "## 2026-01-15")
	assert.Contains(t, result, "**Created** [Use Postgres](/decisions/")
	assert.Contains(t, result, "**Created** [Web Project](/context/")

	// Verify date ordering (newer date first — reverse chronological)
	idx14 := strings.Index(result, "## 2026-01-14")
	idx15 := strings.Index(result, "## 2026-01-15")
	assert.True(t, idx15 < idx14, "dates should be in reverse chronological order (newest first)")
}

func TestSlugifyViaExport(t *testing.T) {
	// Test slugify behavior through full export with a title that needs slugifying
	scopeID := uuid.New()
	id := uuid.MustParse("abcdef12-3456-7890-abcd-ef1234567890")
	repo := &mockRepo{
		memories: []*Memory{
			{
				ID:        id,
				Scope:     ScopeUser,
				ScopeID:   scopeID,
				Type:      "decision",
				Title:     "Hello World! Special @#$ Chars",
				Content:   "Test content",
				Timestamp: time.Now(),
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
		},
	}

	exporter := NewExporter(repo)
	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// Check that the concept doc filename is properly slugified
	found := false
	for name := range files {
		if strings.Contains(name, "hello-world-special-chars") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected slugified filename, got files: %v", fileNames(files))
}

func TestExportContextCancellation(t *testing.T) {
	scopeID := uuid.New()
	now := time.Now().UTC()

	// Create many memories so the export takes time
	var memories []*Memory
	for i := 0; i < 10; i++ {
		memories = append(memories, &Memory{
			ID:        uuid.New(),
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "decision",
			Title:     "Slow Decision",
			Content:   "Content",
			Tags:      StringArray{"test"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}

	repo := &slowMockRepo{
		mockRepo: mockRepo{memories: memories},
		delay:    500 * time.Millisecond,
	}
	exporter := NewExporter(repo)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var buf bytes.Buffer
	err := exporter.Export(ctx, ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
	}, &buf)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestExportWithTypeAndTagFilter(t *testing.T) {
	scopeID := uuid.New()
	now := time.Now().UTC()
	ids := newTestUUIDs(6)

	memories := []*Memory{
		{
			// decision + database tag -> should match
			ID:        ids[0],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "decision",
			Title:     "Use PostgreSQL",
			Content:   "Chose Postgres for reliability",
			Tags:      StringArray{"database", "postgres"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			// decision + language tag -> should NOT match (wrong tag)
			ID:        ids[1],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "decision",
			Title:     "Use Go for Backend",
			Content:   "Go for performance",
			Tags:      StringArray{"language", "go"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			// context + database tag -> should NOT match (wrong type)
			ID:        ids[2],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "context",
			Title:     "Database Context",
			Content:   "Background info",
			Tags:      StringArray{"database"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			// decision + database tag -> should match
			ID:        ids[3],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "decision",
			Title:     "Use Redis for Cache",
			Content:   "Redis for caching layer",
			Tags:      StringArray{"database", "cache"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			// progress + database tag -> should NOT match (wrong type)
			ID:        ids[4],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "progress",
			Title:     "Database Setup Progress",
			Content:   "Progress update",
			Tags:      StringArray{"database"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			// decision + project tag -> should NOT match (wrong tag)
			ID:        ids[5],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "decision",
			Title:     "Project Structure",
			Content:   "Project structure decision",
			Tags:      StringArray{"project"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}

	repo := &mockRepo{memories: memories}
	exporter := NewExporter(repo)

	var buf bytes.Buffer
	err := exporter.Export(context.Background(), ExportOptions{
		Scope:   ScopeUser,
		ScopeID: scopeID,
		Types:   []string{"decision"},
		Tags:    []string{"database"},
	}, &buf)
	require.NoError(t, err)

	files, err := extractTarGz(buf.Bytes())
	require.NoError(t, err)

	// Should only have 2 concept docs: "Use PostgreSQL" and "Use Redis for Cache"
	conceptCount := 0
	for name := range files {
		if strings.HasSuffix(name, ".md") && !strings.HasSuffix(name, "/index.md") {
			conceptCount++
			assert.Contains(t, name, "/decisions/",
				"type filter should only include decisions/, got: %s", name)
		}
	}
	assert.Equal(t, 2, conceptCount,
		"expected 2 docs (decision + database tag), got files: %v", fileNames(files))

	// Verify the correct memories are present
	foundPostgres := false
	foundRedis := false
	for name := range files {
		if strings.Contains(name, "use-postgresql") {
			foundPostgres = true
		}
		if strings.Contains(name, "use-redis-for-cache") {
			foundRedis = true
		}
	}
	assert.True(t, foundPostgres, "expected 'use-postgresql' in export")
	assert.True(t, foundRedis, "expected 'use-redis-for-cache' in export")
}
