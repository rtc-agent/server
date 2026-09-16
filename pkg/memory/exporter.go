package memory

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// ExportOptions defines options for exporting memories
type ExportOptions struct {
	Scope        ScopeType
	ScopeID      uuid.UUID
	Types        []string // filter by multiple types
	Tags         []string // filter by tags
	IncludeLinks bool     // reserved: include cross-reference links between concept docs
	IncludeLog   bool
}

// Exporter exports memories as OKF v0.2 compliant tar.gz bundles
type Exporter struct {
	repo      Repository
	formatter *Formatter
}

// NewExporter creates a new Exporter
func NewExporter(repo Repository) *Exporter {
	return &Exporter{
		repo:      repo,
		formatter: NewFormatter(),
	}
}

// typeToDirectory maps a memory type to a directory name in the OKF bundle
func typeToDirectory(typ string) string {
	switch typ {
	case "decision":
		return "decisions"
	case "context":
		return "context"
	case "progress":
		return "progress"
	case "issue":
		return "issues"
	case "learnings":
		return "learnings"
	case "user":
		return "user"
	case "feedback":
		return "feedback"
	case "project":
		return "project"
	case "reference":
		return "references"
	default:
		return "misc"
	}
}

// titleCase converts a string to title case without using deprecated strings.Title
func titleCase(s string) string {
	prev := ' '
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(prev) || prev == '-' || prev == '_' {
			prev = r
			return unicode.ToUpper(r)
		}
		prev = r
		return r
	}, s)
}

// slugify converts a title to a URL/filename-safe slug.
// Non-ASCII letters (e.g. CJK characters) are stripped, not transliterated.
func slugify(s string) string {
	var b strings.Builder
	prevDash := true // start true to avoid leading dash
	for _, r := range s {
		if isASCIILetterOrDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			prevDash = false
		} else if !prevDash {
			b.WriteRune('-')
			prevDash = true
		}
	}
	result := strings.TrimRight(b.String(), "-")
	if result == "" {
		return "untitled"
	}
	return result
}

// isASCIILetterOrDigit returns true for ASCII letters (a-z, A-Z) and digits (0-9)
func isASCIILetterOrDigit(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// writeFile writes a file entry into a tar writer
func writeFile(tw *tar.Writer, name string, data []byte) error {
	header := &tar.Header{
		Name:    name,
		Size:    int64(len(data)),
		Mode:    0644,
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write tar header for %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write tar content for %s: %w", name, err)
	}
	return nil
}

// Export exports memories as an OKF v0.2 compliant tar.gz bundle to the writer
func (e *Exporter) Export(ctx context.Context, opts ExportOptions, w io.Writer) error {
	memories, err := e.loadMemories(ctx, opts)
	if err != nil {
		return fmt.Errorf("load memories: %w", err)
	}

	// Sort memories by type then by created time for deterministic output
	sort.Slice(memories, func(i, j int) bool {
		if memories[i].Type != memories[j].Type {
			return memories[i].Type < memories[j].Type
		}
		return memories[i].CreatedAt.Before(memories[j].CreatedAt)
	})

	bundleName := fmt.Sprintf("%s-%s-%s",
		string(opts.Scope),
		uuidShort(opts.ScopeID),
		time.Now().Format("20060102-150405"),
	)

	gzWriter := gzip.NewWriter(w)
	tw := tar.NewWriter(gzWriter)

	if err := e.writeBundle(ctx, tw, bundleName, opts, memories); err != nil {
		_ = tw.Close()
		_ = gzWriter.Close()
		return err
	}

	// Close in order: tar first (flushes to gzip), then gzip (flushes to writer).
	// Both must succeed for the archive to be valid.
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return fmt.Errorf("close gzip writer: %w", err)
	}
	return nil
}

// writeBundle writes all bundle entries into the tar writer.
func (e *Exporter) writeBundle(ctx context.Context, tw *tar.Writer, bundleName string, opts ExportOptions, memories []*Memory) error {
	// Write index.md
	indexContent := e.generateIndex(opts, memories)
	if err := writeFile(tw, bundleName+"/index.md", []byte(indexContent)); err != nil {
		return err
	}

	// Write log.md if requested
	if opts.IncludeLog {
		logContent := e.generateLog(memories)
		if err := writeFile(tw, bundleName+"/log.md", []byte(logContent)); err != nil {
			return err
		}
	}

	// Write per-memory concept docs
	for _, m := range memories {
		// Check for context cancellation before processing each memory.
		// This prevents wasted work when the client disconnects mid-export.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("export cancelled: %w", err)
		}

		dir := typeToDirectory(m.Type)
		filename := fmt.Sprintf("%s-%s.md", uuidShort(m.ID), slugify(m.Title))
		path := bundleName + "/" + dir + "/" + filename

		content := e.formatter.FormatForExport(m)
		if err := writeFile(tw, path, []byte(content)); err != nil {
			return err
		}
	}

	return nil
}

// loadMemories fetches memories from the repo based on export options
func (e *Exporter) loadMemories(ctx context.Context, opts ExportOptions) ([]*Memory, error) {
	listOpts := ListOptions{}

	if len(opts.Types) > 0 {
		// Use batch IN query instead of N+1 queries
		listOpts.Types = opts.Types
	}

	memories, err := e.repo.ListByScope(ctx, opts.Scope, opts.ScopeID, listOpts)
	if err != nil {
		return nil, err
	}
	return filterByTags(memories, opts.Tags), nil
}

// filterByTags filters memories that have at least one of the given tags
func filterByTags(memories []*Memory, tags []string) []*Memory {
	if len(tags) == 0 {
		return memories
	}
	tagSet := make(map[string]bool, len(tags))
	for _, t := range tags {
		tagSet[t] = true
	}
	var result []*Memory
	for _, m := range memories {
		for _, mt := range m.Tags {
			if tagSet[mt] {
				result = append(result, m)
				break
			}
		}
	}
	return result
}

// generateIndex generates the index.md content for the OKF bundle
func (e *Exporter) generateIndex(opts ExportOptions, memories []*Memory) string {
	var b strings.Builder

	// OKF version header
	b.WriteString("---\n")
	b.WriteString("okf_version: \"0.2\"\n")
	b.WriteString("---\n\n")

	// Title and scope info
	scopeStr := string(opts.Scope)
	scopeIDStr := opts.ScopeID.String()
	fmt.Fprintf(&b, "# %s %s Memory Bundle\n\n", titleCase(scopeStr), uuidShort(opts.ScopeID))
	fmt.Fprintf(&b, "- **Scope**: %s\n", scopeStr)
	fmt.Fprintf(&b, "- **Scope ID**: %s\n", scopeIDStr)
	fmt.Fprintf(&b, "- **Generated**: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Total memories**: %d\n", len(memories))
	b.WriteString("\n")

	// Group memories by type
	grouped := groupByType(memories)
	types := sortedTypeKeys(grouped)

	b.WriteString("## Contents\n\n")
	for _, typ := range types {
		mems := grouped[typ]
		dir := typeToDirectory(typ)
		fmt.Fprintf(&b, "### %s (%d)\n\n", titleCase(typ), len(mems))
		for _, m := range mems {
			filename := fmt.Sprintf("%s-%s.md", uuidShort(m.ID), slugify(m.Title))
			fmt.Fprintf(&b, "- [%s](/%s/%s)\n", m.Title, dir, filename)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// generateLog generates the log.md content with creation dates grouped
func (e *Exporter) generateLog(memories []*Memory) string {
	var b strings.Builder

	b.WriteString("---\n")
	b.WriteString("okf_version: \"0.2\"\n")
	b.WriteString("---\n\n")

	b.WriteString("# Memory Log\n\n")

	// Group by date
	byDate := make(map[string][]*Memory)
	var dates []string
	for _, m := range memories {
		dateStr := m.CreatedAt.Format("2006-01-02")
		if _, ok := byDate[dateStr]; !ok {
			dates = append(dates, dateStr)
		}
		byDate[dateStr] = append(byDate[dateStr], m)
	}
	sort.Strings(dates)
	// Reverse to get descending order (newest first), as specified by OKF log.md convention.
	for i, j := 0, len(dates)-1; i < j; i, j = i+1, j-1 {
		dates[i], dates[j] = dates[j], dates[i]
	}

	for _, date := range dates {
		mems := byDate[date]
		fmt.Fprintf(&b, "## %s\n\n", date)
		for _, m := range mems {
			dir := typeToDirectory(m.Type)
			filename := fmt.Sprintf("%s-%s.md", uuidShort(m.ID), slugify(m.Title))
			// Use forward slashes for bundle-relative paths (portable across platforms).
			path := fmt.Sprintf("/%s/%s", dir, filename)
			fmt.Fprintf(&b, "- **Created** [%s](%s)\n", m.Title, path)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// groupByType groups memories by their type
func groupByType(memories []*Memory) map[string][]*Memory {
	result := make(map[string][]*Memory)
	for _, m := range memories {
		result[m.Type] = append(result[m.Type], m)
	}
	return result
}

// sortedTypeKeys returns sorted keys of a type-grouped map
func sortedTypeKeys(m map[string][]*Memory) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// uuidShort returns the first 8 characters of a UUID's string representation
func uuidShort(id uuid.UUID) string {
	s := id.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
