package usecase

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// ============================================================
// Tests for singleflight context isolation in LookupCredential.
//
// The core property: when the leader request's context is cancelled,
// the underlying DB query (running with background context) continues,
// and other waiters receive the result normally.
// ============================================================

// mockCredRepo is a fake TemporaryCredentialRepo with controllable latency.
type mockCredRepo struct {
	cred      *model.TemporaryCredential
	err       error
	callCount atomic.Int32

	// Signals that GetByAccessKeyID has started executing.
	started chan struct{}
	// Blocks GetByAccessKeyID until the test releases it.
	unblock chan struct{}
}

func (m *mockCredRepo) Create(_ context.Context, _ *model.TemporaryCredential) error {
	return nil
}

func (m *mockCredRepo) GetByAccessKeyID(ctx context.Context, _ string) (*model.TemporaryCredential, error) {
	m.callCount.Add(1)
	if m.started != nil {
		close(m.started)
	}
	if m.unblock != nil {
		select {
		case <-m.unblock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return m.cred, m.err
}

func (m *mockCredRepo) FindExpired(_ context.Context, _ time.Time) ([]*model.TemporaryCredential, error) {
	return nil, nil
}

func (m *mockCredRepo) Delete(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (m *mockCredRepo) DeleteExpired(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// Compile-time interface check.
var _ repo.TemporaryCredentialRepo = (*mockCredRepo)(nil)

// newCredCacheTestUsecase wires a OSS3Usecase with the given mock repo and a
// fresh miniredis instance. The caller is responsible for closing Redis if
// needed (miniredis handles cleanup via t.Cleanup when created with RunT).
func newCredCacheTestUsecase(t *testing.T, mock *mockCredRepo, mr *miniredis.Miniredis) *OSS3Usecase {
	t.Helper()
	uc := newTestUsecase(t)
	uc.credRepo = mock
	uc.redis = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return uc
}

// validTestCredential returns a credential that expires in 1 hour.
// Note: SecretAccessKey is returned in plaintext for test assertions.
// The caller must encrypt it before storing in the mock DB.
func validTestCredential(accessKeyID string) *model.TemporaryCredential {
	return &model.TemporaryCredential{
		ID:              uuid.New(),
		UserID:          "user-test",
		AccessKeyID:     accessKeyID,
		SecretAccessKey: "secret-key",
		SessionToken:    "session-token",
		ExpiresAt:       time.Now().Add(1 * time.Hour),
	}
}

// encryptTestCredential encrypts the SecretAccessKey for DB storage in tests.
// It modifies the credential in place via the pointer.
func encryptTestCredential(t *testing.T, uc *OSS3Usecase, cred *model.TemporaryCredential) {
	t.Helper()
	encrypted, err := uc.encryptString(cred.SecretAccessKey)
	if err != nil {
		t.Fatalf("encrypt secret key: %v", err)
	}
	cred.SecretAccessKey = encrypted
}

// ---------- Tests ----------

// TestLookupCredential_ContextCancelled_DBQueryContinues verifies the core
// singleflight fix: when the leader request is cancelled, the DB query
// (running with background context) is NOT interrupted, and other waiters
// still receive the result.
func TestLookupCredential_ContextCancelled_DBQueryContinues(t *testing.T) {
	mr := miniredis.RunT(t)
	cred := validTestCredential("AK_TEST_CTXT")
	mock := &mockCredRepo{
		cred:    cred,
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	uc := newCredCacheTestUsecase(t, mock, mr)
	encryptTestCredential(t, uc, cred)

	// Request 1 (leader): cancellable context.
	ctx1, cancel1 := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var result1 *credentialCacheValue
	var err1 error

	wg.Add(1)
	go func() {
		defer wg.Done()
		result1, err1 = uc.LookupCredential(ctx1, "AK_TEST_CTXT")
	}()

	// Wait until the DB query actually starts (singleflight is executing).
	<-mock.started

	// Request 2: independent context, same access key → joins singleflight.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	var result2 *credentialCacheValue
	var err2 error
	wg.Add(1)
	go func() {
		defer wg.Done()
		result2, err2 = uc.LookupCredential(ctx2, "AK_TEST_CTXT")
	}()

	// Give goroutine 2 time to enter singleflight.DoChan and block on <ch.
	time.Sleep(100 * time.Millisecond)

	// Cancel request 1. The DB query must continue (background context).
	cancel1()

	// Release the DB query.
	close(mock.unblock)

	wg.Wait()

	// Request 1 was cancelled → expects context error.
	if err1 != context.Canceled {
		t.Errorf("Request 1: expected context.Canceled, got %v", err1)
	}
	if result1 != nil {
		t.Errorf("Request 1: expected nil result, got %+v", result1)
	}

	// Request 2 should succeed despite request 1 being cancelled.
	if err2 != nil {
		t.Fatalf("Request 2: unexpected error: %v", err2)
	}
	if result2 == nil {
		t.Fatal("Request 2: expected credential, got nil")
	}
	if result2.UserID != "user-test" {
		t.Errorf("Request 2: UserID = %q, want %q", result2.UserID, "user-test")
	}

	// singleflight should have deduplicated: exactly 1 DB call.
	if got := mock.callCount.Load(); got != 1 {
		t.Errorf("DB call count = %d, want 1 (singleflight dedup)", got)
	}
}

// TestLookupCredential_Success verifies the happy path: cache miss → DB query
// → populate cache → return credential.
func TestLookupCredential_Success(t *testing.T) {
	mr := miniredis.RunT(t)
	cred := validTestCredential("AK_TEST_OK")
	mock := &mockCredRepo{cred: cred}
	uc := newCredCacheTestUsecase(t, mock, mr)
	encryptTestCredential(t, uc, cred)

	ctx := context.Background()
	result, err := uc.LookupCredential(ctx, "AK_TEST_OK")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected credential, got nil")
	}
	if result.UserID != "user-test" {
		t.Errorf("UserID = %q, want %q", result.UserID, "user-test")
	}
	if result.SecretAccessKey != "secret-key" {
		t.Errorf("SecretAccessKey = %q, want %q", result.SecretAccessKey, "secret-key")
	}
	if mock.callCount.Load() != 1 {
		t.Errorf("DB call count = %d, want 1", mock.callCount.Load())
	}
}

// TestLookupCredential_CacheHit verifies that a valid cache entry is returned
// without hitting the DB.
func TestLookupCredential_CacheHit(t *testing.T) {
	mr := miniredis.RunT(t)
	cred := validTestCredential("AK_TEST_CACHE")
	mock := &mockCredRepo{cred: cred}
	uc := newCredCacheTestUsecase(t, mock, mr)
	encryptTestCredential(t, uc, cred)

	ctx := context.Background()

	// First call: populates the cache.
	result1, err := uc.LookupCredential(ctx, "AK_TEST_CACHE")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if result1 == nil {
		t.Fatal("first call: expected credential")
	}
	if mock.callCount.Load() != 1 {
		t.Fatalf("after first call: DB count = %d, want 1", mock.callCount.Load())
	}

	// Second call: should hit cache, no additional DB query.
	result2, err := uc.LookupCredential(ctx, "AK_TEST_CACHE")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if result2 == nil {
		t.Fatal("second call: expected credential from cache")
	}
	if result2.UserID != "user-test" {
		t.Errorf("second call: UserID = %q, want %q", result2.UserID, "user-test")
	}
	if mock.callCount.Load() != 1 {
		t.Errorf("after second call: DB count = %d, want 1 (cache hit)", mock.callCount.Load())
	}
}

// TestLookupCredential_DBError verifies that a DB error propagates to the caller.
func TestLookupCredential_DBError(t *testing.T) {
	mr := miniredis.RunT(t)
	mock := &mockCredRepo{
		err: context.DeadlineExceeded, // simulate a DB timeout
	}
	uc := newCredCacheTestUsecase(t, mock, mr)

	ctx := context.Background()
	result, err := uc.LookupCredential(ctx, "AK_TEST_ERR")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
}

// TestLookupCredential_NotFoundInDB verifies that a DB "not found" (nil, nil
// from repo) returns (nil, nil) from LookupCredential.
func TestLookupCredential_NotFoundInDB(t *testing.T) {
	mr := miniredis.RunT(t)
	mock := &mockCredRepo{
		cred: nil, // repo returns (nil, nil) for not found
		err:  nil,
	}
	uc := newCredCacheTestUsecase(t, mock, mr)

	ctx := context.Background()
	result, err := uc.LookupCredential(ctx, "AK_TEST_MISS")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result for not-found, got %+v", result)
	}
}

// TestLookupCredential_ExpiredInDB verifies that a credential already expired
// in the DB is treated as not found.
func TestLookupCredential_ExpiredInDB(t *testing.T) {
	mr := miniredis.RunT(t)
	cred := &model.TemporaryCredential{
		ID:              uuid.New(),
		UserID:          "user-test",
		AccessKeyID:     "AK_TEST_EXPIRED",
		SecretAccessKey: "secret",
		SessionToken:    "token",
		ExpiresAt:       time.Now().Add(-1 * time.Hour), // already expired
	}
	mock := &mockCredRepo{
		cred: cred,
	}
	uc := newCredCacheTestUsecase(t, mock, mr)
	encryptTestCredential(t, uc, cred)

	ctx := context.Background()
	result, err := uc.LookupCredential(ctx, "AK_TEST_EXPIRED")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil for expired credential, got %+v", result)
	}
}

// TestLookupCredential_MultipleWaiters_AllGetResult verifies that when many
// requests share a singleflight and the leader is cancelled, ALL remaining
// waiters still get the result.
func TestLookupCredential_MultipleWaiters_AllGetResult(t *testing.T) {
	mr := miniredis.RunT(t)
	cred := validTestCredential("AK_TEST_MULTI")
	mock := &mockCredRepo{
		cred:    cred,
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	uc := newCredCacheTestUsecase(t, mock, mr)
	encryptTestCredential(t, uc, cred)

	const numWaiters = 5
	ctxLeader, cancelLeader := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	type outcome struct {
		result *credentialCacheValue
		err    error
	}
	results := make([]outcome, numWaiters+1) // index 0 = leader

	// Leader
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, e := uc.LookupCredential(ctxLeader, "AK_TEST_MULTI")
		results[0] = outcome{r, e}
	}()

	<-mock.started

	// Waiters (indices 1..numWaiters)
	for i := 1; i <= numWaiters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, e := uc.LookupCredential(ctx, "AK_TEST_MULTI")
			results[idx] = outcome{r, e}
		}(i)
	}

	// Let all waiters enter singleflight.
	time.Sleep(200 * time.Millisecond)

	// Cancel leader, then release DB.
	cancelLeader()
	close(mock.unblock)

	wg.Wait()

	// Leader should have been cancelled.
	if results[0].err != context.Canceled {
		t.Errorf("leader: expected context.Canceled, got %v", results[0].err)
	}

	// All waiters should have succeeded.
	for i := 1; i <= numWaiters; i++ {
		if results[i].err != nil {
			t.Errorf("waiter %d: unexpected error: %v", i, results[i].err)
		}
		if results[i].result == nil {
			t.Errorf("waiter %d: expected credential, got nil", i)
		}
	}

	// Exactly 1 DB call (singleflight dedup).
	if got := mock.callCount.Load(); got != 1 {
		t.Errorf("DB call count = %d, want 1", got)
	}
}
