package util

import "maps"

// CloneWith clones src, writes a single KV pair, and returns the new map.
// If src is nil, returns a new map containing only that KV pair.
// Purpose: avoid concurrent-write races when multiple goroutines share a map.
func CloneWith[K comparable, V any](src map[K]V, key K, value V) map[K]V {
	dst := make(map[K]V, len(src)+1)
	maps.Copy(dst, src)
	dst[key] = value
	return dst
}
