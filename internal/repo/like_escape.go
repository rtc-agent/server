package repo

import "strings"

// escapeLikePattern escapes LIKE/ILIKE meta-characters in user input
// to prevent wildcard injection attacks.
//
// Without escaping:
//   - query = "%" matches all records (excessive data exposure)
//   - query = "______________" triggers expensive pattern matching (DoS)
//
// Usage:
//
//	likeQuery := "%" + escapeLikePattern(query) + "%"
func escapeLikePattern(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
