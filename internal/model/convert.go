package model

// Utility functions for pointer operations.
// TODO: Consider moving to internal/infra/httputil or a dedicated util package.

// StrPtr converts a non-empty string to *string; returns nil for empty strings.
func StrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// DerefStr dereferences a *string; returns empty string for nil.
func DerefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ptrTo returns a pointer to v (generic version).
func ptrTo[T any](v T) *T { return &v }
