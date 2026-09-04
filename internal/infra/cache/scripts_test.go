package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestWorkerScriptsNotNil(t *testing.T) {
	type namedScript struct {
		name   string
		script *redis.Script
	}
	scripts := []namedScript{
		{"WorkerRegister", WorkerRegister},
		{"WorkerHeartbeat", WorkerHeartbeat},
		{"WorkerDeregister", WorkerDeregister},
		{"SessionAssign", SessionAssign},
		{"SessionReassign", SessionReassign},
		{"TurnEnqueue", TurnEnqueue},
		{"AppendChunk", AppendChunk},
	}
	for _, s := range scripts {
		if s.script == nil {
			t.Errorf("%s is nil", s.name)
			continue
		}
		if h := s.script.Hash(); h == "" {
			t.Errorf("%s has empty Hash", s.name)
		}
	}
}

func TestAppendChunk_Script(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	ctx := context.Background()
	key := "message:stream:test-msg-001"
	ttlSeconds := int64(300)

	// First append: should create list with 1 element
	result, err := AppendChunk.Run(ctx, rdb, []string{key}, ttlSeconds, "chunk-A").Int64()
	if err != nil {
		t.Fatalf("AppendChunk (1st) failed: %v", err)
	}
	if result != 1 {
		t.Errorf("expected length 1 after first append, got %d", result)
	}

	// Verify TTL is set
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("TTL failed: %v", err)
	}
	if ttl.Seconds() < float64(ttlSeconds-1) || ttl.Seconds() > float64(ttlSeconds+1) {
		t.Errorf("expected TTL ~%ds, got %v", ttlSeconds, ttl)
	}

	// Second append: should increase list length to 2
	result, err = AppendChunk.Run(ctx, rdb, []string{key}, ttlSeconds, "chunk-B").Int64()
	if err != nil {
		t.Fatalf("AppendChunk (2nd) failed: %v", err)
	}
	if result != 2 {
		t.Errorf("expected length 2 after second append, got %d", result)
	}

	// Verify list contents
	vals, err := rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		t.Fatalf("LRange failed: %v", err)
	}
	if len(vals) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(vals))
	}
	if vals[0] != "chunk-A" || vals[1] != "chunk-B" {
		t.Errorf("unexpected list contents: %v", vals)
	}

	// Third append and verify TTL is refreshed
	mr.FastForward(4 * time.Minute)
	result, err = AppendChunk.Run(ctx, rdb, []string{key}, ttlSeconds, "chunk-C").Int64()
	if err != nil {
		t.Fatalf("AppendChunk (3rd) failed: %v", err)
	}
	if result != 3 {
		t.Errorf("expected length 3 after third append, got %d", result)
	}

	// TTL should be refreshed
	ttl, err = rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("TTL after refresh failed: %v", err)
	}
	if ttl.Minutes() < 4.5 {
		t.Errorf("expected TTL ~5min after refresh, got %v", ttl)
	}

	// Verify all 3 elements
	vals, err = rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		t.Fatalf("LRange after 3rd append failed: %v", err)
	}
	expected := []string{"chunk-A", "chunk-B", "chunk-C"}
	if len(vals) != 3 {
		t.Fatalf("expected 3 elements, got %d: %v", len(vals), vals)
	}
	for i, want := range expected {
		if vals[i] != want {
			t.Errorf("vals[%d] = %q, want %q", i, vals[i], want)
		}
	}
}
