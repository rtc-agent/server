package memory

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ─── mock repo ───

// mockRepo is an in-memory implementation of Repository for testing.
type mockRepo struct {
	memories []*Memory
}

func (r *mockRepo) Create(_ context.Context, m *Memory) error {
	r.memories = append(r.memories, m)
	return nil
}

func (r *mockRepo) BatchCreate(_ context.Context, memories []*Memory) error {
	r.memories = append(r.memories, memories...)
	return nil
}

func (r *mockRepo) GetByID(_ context.Context, id uuid.UUID) (*Memory, error) {
	for _, m := range r.memories {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, ErrNotFound
}

func (r *mockRepo) ListByScope(_ context.Context, scope ScopeType, scopeID uuid.UUID, opts ListOptions) ([]*Memory, error) {
	// Build type filter set from both singular Type and plural Types fields.
	typeSet := make(map[string]bool)
	if opts.Type != "" {
		typeSet[opts.Type] = true
	}
	for _, t := range opts.Types {
		typeSet[t] = true
	}

	var result []*Memory
	for _, m := range r.memories {
		if m.Scope != scope || m.ScopeID != scopeID {
			continue
		}
		if len(typeSet) > 0 && !typeSet[m.Type] {
			continue
		}
		result = append(result, m)
	}
	return result, nil
}

func (r *mockRepo) ListRecentForInjection(_ context.Context, scope ScopeType, scopeID uuid.UUID, maxCount, maxTokens int) ([]*Memory, error) {
	return nil, nil
}

func (r *mockRepo) Search(_ context.Context, scope ScopeType, scopeID uuid.UUID, query string, limit int) ([]*Memory, error) {
	var result []*Memory
	for _, m := range r.memories {
		if m.Scope != scope || m.ScopeID != scopeID {
			continue
		}
		if strings.Contains(m.Title, query) || strings.Contains(m.Content, query) {
			result = append(result, m)
		}
	}
	return result, nil
}

func (r *mockRepo) Update(_ context.Context, id uuid.UUID, fields map[string]any) error {
	return nil
}

func (r *mockRepo) Delete(_ context.Context, id uuid.UUID) error {
	for i, m := range r.memories {
		if m.ID == id {
			r.memories = append(r.memories[:i], r.memories[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (r *mockRepo) DeleteByScope(_ context.Context, scope ScopeType, scopeID uuid.UUID) error {
	return nil
}

func (r *mockRepo) GetLinked(_ context.Context, id uuid.UUID, relation string) ([]*Memory, error) {
	return nil, nil
}

func (r *mockRepo) CreateLink(_ context.Context, link *MemoryLink) error {
	return nil
}

func (r *mockRepo) DeleteLink(_ context.Context, fromID, toID uuid.UUID) error {
	return nil
}

func (r *mockRepo) CountTokensByScope(_ context.Context, scope ScopeType, scopeID uuid.UUID) (int, error) {
	return 0, nil
}

// ─── slow mock repo (for context cancellation testing) ───

// slowMockRepo is like mockRepo but adds configurable delays to simulate I/O.
type slowMockRepo struct {
	mockRepo
	delay time.Duration
}

func (r *slowMockRepo) ListByScope(ctx context.Context, scope ScopeType, scopeID uuid.UUID, opts ListOptions) ([]*Memory, error) {
	select {
	case <-time.After(r.delay):
		// proceed normally
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.mockRepo.ListByScope(ctx, scope, scopeID, opts)
}

// ─── test helpers ───

func newTestUUIDs(n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}
	return ids
}

func newSampleMemories(scopeID uuid.UUID) []*Memory {
	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	ids := newTestUUIDs(4)
	return []*Memory{
		{
			ID:        ids[0],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "decision",
			Title:     "Use PostgreSQL",
			Content:   "We chose PostgreSQL for its reliability",
			Tags:      StringArray{"database", "postgres"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        ids[1],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "context",
			Title:     "Project Background",
			Content:   "This is a web application project",
			Tags:      StringArray{"project"},
			Timestamp: now.Add(-24 * time.Hour),
			CreatedAt: now.Add(-24 * time.Hour),
			UpdatedAt: now.Add(-24 * time.Hour),
		},
		{
			ID:        ids[2],
			Scope:     ScopeUser,
			ScopeID:   scopeID,
			Type:      "decision",
			Title:     "Use Go for Backend",
			Content:   "Go provides great performance",
			Tags:      StringArray{"language", "go"},
			Timestamp: now.Add(-48 * time.Hour),
			CreatedAt: now.Add(-48 * time.Hour),
			UpdatedAt: now.Add(-48 * time.Hour),
		},
		{
			ID:        ids[3],
			Scope:     ScopeSession,
			ScopeID:   uuid.New(), // different scopeID
			Type:      "progress",
			Title:     "Completed Auth Module",
			Content:   "Authentication module is done",
			Tags:      StringArray{"auth"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
}

func extractTarGz(data []byte) (map[string]string, error) {
	gzReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gzReader.Close() }()

	files := make(map[string]string)
	tr := tar.NewReader(gzReader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[hdr.Name] = string(content)
	}
	return files, nil
}

func fileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	return names
}
