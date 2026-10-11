package model

// StrPtr converts a non-empty string to *string; returns nil for empty strings.
// NOTE: A copy of this function also exists in internal/infra/httputil/ptr.go
// for use by handler packages that must not import internal/model.
func StrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// DerefStr dereferences a *string; returns empty string for nil.
// NOTE: A copy of this function also exists in internal/infra/httputil/ptr.go
// for use by handler packages that must not import internal/model.
func DerefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ptrTo returns a pointer to v (generic version).
func ptrTo[T any](v T) *T { return &v }
