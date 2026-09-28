package util

import "maps"

// CloneWith 克隆 src 并写入一对 KV，返回新 map。
// 若 src 为 nil 则返回仅含该 KV 的新 map。
// 用途：避免多个 goroutine 共享同一 map 导致的并发写入问题。
func CloneWith[K comparable, V any](src map[K]V, key K, value V) map[K]V {
	dst := make(map[K]V, len(src)+1)
	maps.Copy(dst, src)
	dst[key] = value
	return dst
}
