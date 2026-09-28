package util

import (
	"testing"
)

func TestCloneWith_NilSrc(t *testing.T) {
	result := CloneWith(nil, "key", 42)
	if len(result) != 1 {
		t.Errorf("expected len=1, got %d", len(result))
	}
	if result["key"] != 42 {
		t.Errorf("expected result[\"key\"]=42, got %d", result["key"])
	}
}

func TestCloneWith_NonNilSrc(t *testing.T) {
	src := map[string]int{"a": 1, "b": 2}
	result := CloneWith(src, "c", 3)

	// 原 map 不变
	if len(src) != 2 {
		t.Errorf("src modified: expected len=2, got %d", len(src))
	}
	if src["c"] != 0 {
		t.Errorf("src modified: src[\"c\"] should be 0, got %d", src["c"])
	}

	// 新 map 包含所有 KV
	if len(result) != 3 {
		t.Errorf("expected len=3, got %d", len(result))
	}
	if result["a"] != 1 || result["b"] != 2 || result["c"] != 3 {
		t.Errorf("result mismatch: %v", result)
	}
}

func TestCloneWith_OverrideExistingKey(t *testing.T) {
	src := map[string]int{"key": 1}
	result := CloneWith(src, "key", 99)

	// 原 map 不变
	if src["key"] != 1 {
		t.Errorf("src modified: src[\"key\"] should be 1, got %d", src["key"])
	}

	// 新 map 中 key 被覆盖
	if result["key"] != 99 {
		t.Errorf("expected result[\"key\"]=99, got %d", result["key"])
	}
}
