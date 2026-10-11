// Package httputil provides common HTTP response helpers and utility functions.
package httputil

// StrPtr converts a non-empty string to *string; returns nil for empty strings.
// Useful for optional protocol fields where empty means absent.
func StrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// DerefStr dereferences a *string; returns empty string for nil.
// Useful for reading optional protocol fields safely.
func DerefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
