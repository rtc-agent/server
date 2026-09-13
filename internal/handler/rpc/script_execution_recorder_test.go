// Package rpchandler — scriptExecutionRecorder unit tests.
package rpchandler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ---------------------------------------------------------------------------
// Mock Repo
// ---------------------------------------------------------------------------

type mockScriptExecutionRepo struct {
	createFunc func(ctx context.Context, exec *model.ScriptExecution) error
}

func (m *mockScriptExecutionRepo) Create(ctx context.Context, exec *model.ScriptExecution) error {
	if m.createFunc != nil {
		return m.createFunc(ctx, exec)
	}
	return nil
}

func (m *mockScriptExecutionRepo) GetByRtcID(_ context.Context, _ uuid.UUID) (*model.ScriptExecution, error) {
	return nil, nil
}

func (m *mockScriptExecutionRepo) ListBySession(_ context.Context, _ uuid.UUID, _, _ int) ([]*model.ScriptExecution, int64, error) {
	return nil, 0, nil
}

func (m *mockScriptExecutionRepo) ListByUser(_ context.Context, _ uuid.UUID, _, _ int) ([]*model.ScriptExecution, int64, error) {
	return nil, 0, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newTestRecorder(t *testing.T, repo *mockScriptExecutionRepo) *scriptExecutionRecorder {
	t.Helper()
	deps := &Dependencies{
		ScriptExecutionRepo: repo,
		// Metrics intentionally nil — processTask has a nil check.
	}
	r := newScriptExecutionRecorder(deps, 2, 10)
	t.Cleanup(r.shutdown)
	return r
}

func testRtc(t *testing.T, paramsJSON string) *model.Rtc {
	t.Helper()
	return &model.Rtc{
		ID:         uuid.New(),
		SessionID:  uuid.New(),
		TurnID:     uuid.New(),
		ToolName:   "script",
		Parameters: model.JSONBString(paramsJSON),
		CreatedAt:  time.Now().Truncate(time.Millisecond),
	}
}

func testResultReq(success bool) *protocol.SubmitRtcResultRequest {
	return &protocol.SubmitRtcResultRequest{
		Success: success,
		Result: map[string]any{
			"duration_ms": float64(42),
			"logs":        []any{"hello", "world"},
			"warnings":    []any{"warn1"},
			"errors":      []any{},
		},
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	return contextx.WithClientInfo(context.Background(), uuid.New(), "test-device")
}

// ---------------------------------------------------------------------------
// 3b. processTask unit tests
// ---------------------------------------------------------------------------

func TestProcessTask_Success(t *testing.T) {
	t.Parallel()

	var captured *model.ScriptExecution
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, exec *model.ScriptExecution) error {
			captured = exec
			return nil
		},
	}
	r := newTestRecorder(t, repo)

	params := `{"title":"测试脚本","action":"run","name":"demo","code":"console.log(1)"}`
	rtc := testRtc(t, params)
	req := testResultReq(true)
	ctx := testContext(t)

	r.processTask(scriptRecordTask{ctx: ctx, rtc: rtc, req: req})

	if captured == nil {
		t.Fatal("expected repo.Create to be called")
	}
	if captured.Status != "success" {
		t.Errorf("Status = %q, want %q", captured.Status, "success")
	}
	if captured.Title != "测试脚本" {
		t.Errorf("Title = %q, want %q", captured.Title, "测试脚本")
	}
	if captured.Action != "run" {
		t.Errorf("Action = %q, want %q", captured.Action, "run")
	}
	if captured.ScriptName != "demo" {
		t.Errorf("ScriptName = %q, want %q", captured.ScriptName, "demo")
	}
	if captured.DurationMs != 42 {
		t.Errorf("DurationMs = %d, want 42", captured.DurationMs)
	}
	if len(captured.Logs) != 2 {
		t.Errorf("Logs len = %d, want 2", len(captured.Logs))
	}
	if len(captured.Warnings) != 1 {
		t.Errorf("Warnings len = %d, want 1", len(captured.Warnings))
	}
	if captured.RtcID != rtc.ID {
		t.Errorf("RtcID = %s, want %s", captured.RtcID, rtc.ID)
	}
	if captured.CodeHash == "" {
		t.Error("CodeHash should not be empty when code is present")
	}
	if captured.CodeSize == 0 {
		t.Error("CodeSize should not be zero when code is present")
	}
}

func TestProcessTask_Failed(t *testing.T) {
	t.Parallel()

	var captured *model.ScriptExecution
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, exec *model.ScriptExecution) error {
			captured = exec
			return nil
		},
	}
	r := newTestRecorder(t, repo)

	rtc := testRtc(t, `{"title":"失败测试","action":"eval","code":"throw 1"}`)
	errMsg := "something went wrong"
	req := &protocol.SubmitRtcResultRequest{
		Success: false,
		Error:   &errMsg,
		Result:  nil,
	}
	ctx := testContext(t)

	r.processTask(scriptRecordTask{ctx: ctx, rtc: rtc, req: req})

	if captured == nil {
		t.Fatal("expected repo.Create to be called")
	}
	if captured.Status != "failed" {
		t.Errorf("Status = %q, want %q", captured.Status, "failed")
	}
	if captured.ErrorMessage != "something went wrong" {
		t.Errorf("ErrorMessage = %q, want %q", captured.ErrorMessage, "something went wrong")
	}
}

func TestProcessTask_DefaultAction(t *testing.T) {
	t.Parallel()

	var captured *model.ScriptExecution
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, exec *model.ScriptExecution) error {
			captured = exec
			return nil
		},
	}
	r := newTestRecorder(t, repo)

	// Parameters without "action" field.
	rtc := testRtc(t, `{"title":"默认action","code":"1+1"}`)
	req := testResultReq(true)
	ctx := testContext(t)

	r.processTask(scriptRecordTask{ctx: ctx, rtc: rtc, req: req})

	if captured == nil {
		t.Fatal("expected repo.Create to be called")
	}
	if captured.Action != "eval" {
		t.Errorf("Action = %q, want %q (default)", captured.Action, "eval")
	}
}

func TestProcessTask_EmptyTitle(t *testing.T) {
	t.Parallel()

	var captured *model.ScriptExecution
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, exec *model.ScriptExecution) error {
			captured = exec
			return nil
		},
	}
	r := newTestRecorder(t, repo)

	rtc := testRtc(t, `{"title":"","action":"eval","code":"x"}`)
	req := testResultReq(true)
	ctx := testContext(t)

	// Should not panic.
	r.processTask(scriptRecordTask{ctx: ctx, rtc: rtc, req: req})

	if captured == nil {
		t.Fatal("expected repo.Create to be called")
	}
	if captured.Title != "" {
		t.Errorf("Title = %q, want empty", captured.Title)
	}
}

func TestProcessTask_MalformedParameters(t *testing.T) {
	t.Parallel()

	var captured *model.ScriptExecution
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, exec *model.ScriptExecution) error {
			captured = exec
			return nil
		},
	}
	r := newTestRecorder(t, repo)

	// Malformed JSON — should not panic.
	rtc := testRtc(t, `{not valid json`)
	req := testResultReq(true)
	ctx := testContext(t)

	r.processTask(scriptRecordTask{ctx: ctx, rtc: rtc, req: req})

	if captured == nil {
		t.Fatal("expected repo.Create to be called even with malformed params")
	}
	// Default action should be "eval" since unmarshal failed.
	if captured.Action != "eval" {
		t.Errorf("Action = %q, want %q (default after malformed)", captured.Action, "eval")
	}
}

func TestProcessTask_NilResult(t *testing.T) {
	t.Parallel()

	var captured *model.ScriptExecution
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, exec *model.ScriptExecution) error {
			captured = exec
			return nil
		},
	}
	r := newTestRecorder(t, repo)

	rtc := testRtc(t, `{"title":"nil result","action":"eval","code":"x"}`)
	req := &protocol.SubmitRtcResultRequest{
		Success: true,
		Result:  nil,
	}
	ctx := testContext(t)

	r.processTask(scriptRecordTask{ctx: ctx, rtc: rtc, req: req})

	if captured == nil {
		t.Fatal("expected repo.Create to be called")
	}
	if captured.DurationMs != 0 {
		t.Errorf("DurationMs = %d, want 0 (nil result)", captured.DurationMs)
	}
	if captured.ResultSize != 0 {
		t.Errorf("ResultSize = %d, want 0 (nil result)", captured.ResultSize)
	}
}

func TestProcessTask_CodeHashComputation(t *testing.T) {
	t.Parallel()

	var captured *model.ScriptExecution
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, exec *model.ScriptExecution) error {
			captured = exec
			return nil
		},
	}
	r := newTestRecorder(t, repo)

	code := "console.log('hello world')"
	params := fmt.Sprintf(`{"title":"hash test","action":"eval","code":%s}`, mustJSON(code))
	rtc := testRtc(t, params)
	req := testResultReq(true)
	ctx := testContext(t)

	r.processTask(scriptRecordTask{ctx: ctx, rtc: rtc, req: req})

	if captured == nil {
		t.Fatal("expected repo.Create to be called")
	}

	// Verify hash independently.
	sum := sha256.Sum256([]byte(code))
	expectedHash := hex.EncodeToString(sum[:])
	if captured.CodeHash != expectedHash {
		t.Errorf("CodeHash = %q, want %q", captured.CodeHash, expectedHash)
	}
	if captured.CodeSize != int64(len(code)) {
		t.Errorf("CodeSize = %d, want %d", captured.CodeSize, len(code))
	}
}

func TestProcessTask_DBCreateFails(t *testing.T) {
	t.Parallel()

	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, _ *model.ScriptExecution) error {
			return fmt.Errorf("db connection lost")
		},
	}
	r := newTestRecorder(t, repo)

	rtc := testRtc(t, `{"title":"db fail","action":"eval","code":"x"}`)
	req := testResultReq(true)
	ctx := testContext(t)

	// Should not panic when DB create fails.
	r.processTask(scriptRecordTask{ctx: ctx, rtc: rtc, req: req})
}

// ---------------------------------------------------------------------------
// 3c. Worker Pool behavior tests
// ---------------------------------------------------------------------------

func TestRecorder_SubmitProcessesTask(t *testing.T) {
	t.Parallel()

	var called atomic.Int32
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, _ *model.ScriptExecution) error {
			called.Add(1)
			return nil
		},
	}
	r := newTestRecorder(t, repo)

	rtc := testRtc(t, `{"title":"submit test","action":"eval","code":"x"}`)
	req := testResultReq(true)
	ctx := testContext(t)

	r.submit(ctx, rtc, req)

	// Wait for async processing.
	deadline := time.After(2 * time.Second)
	for called.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for repo.Create to be called")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestRecorder_ChannelFullDropsTask(t *testing.T) {
	t.Parallel()

	// Create recorder with bufferSize=1 and 1 worker.
	// Block the worker by making Create hang, then submit should drop.
	blocker := make(chan struct{})
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, _ *model.ScriptExecution) error {
			<-blocker
			return nil
		},
	}
	deps := &Dependencies{ScriptExecutionRepo: repo}
	r := newScriptExecutionRecorder(deps, 1, 1)
	t.Cleanup(func() {
		close(blocker)
		r.shutdown()
	})

	ctx := testContext(t)
	rtc := testRtc(t, `{"title":"t1","action":"eval","code":"x"}`)
	req := testResultReq(true)

	// First submit fills the worker.
	r.submit(ctx, rtc, req)
	time.Sleep(20 * time.Millisecond) // let worker pick it up

	// Second submit fills the channel buffer.
	r.submit(ctx, rtc, req)

	// Third submit should be dropped (non-blocking).
	// Should not block or panic.
	done := make(chan struct{})
	go func() {
		r.submit(ctx, rtc, req)
		close(done)
	}()
	select {
	case <-done:
		// OK — non-blocking.
	case <-time.After(time.Second):
		t.Fatal("submit() blocked when channel is full — should be non-blocking")
	}
}

func TestRecorder_ShutdownDrainsTasks(t *testing.T) {
	t.Parallel()

	var processed atomic.Int32
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, _ *model.ScriptExecution) error {
			processed.Add(1)
			return nil
		},
	}

	deps := &Dependencies{ScriptExecutionRepo: repo}
	r := newScriptExecutionRecorder(deps, 2, 100)

	ctx := testContext(t)
	rtc := testRtc(t, `{"title":"drain test","action":"eval","code":"x"}`)
	req := testResultReq(true)

	// Submit 10 tasks.
	for i := 0; i < 10; i++ {
		r.submit(ctx, rtc, req)
	}

	// Shutdown should drain all tasks.
	r.shutdown()

	if got := processed.Load(); got != 10 {
		t.Errorf("processed %d tasks, want 10", got)
	}
}

func TestRecorder_WorkerPanicRecovery(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, _ *model.ScriptExecution) error {
			n := callCount.Add(1)
			if n == 1 {
				panic("simulated panic in processTask")
			}
			return nil
		},
	}
	r := newTestRecorder(t, repo)

	ctx := testContext(t)
	rtc := testRtc(t, `{"title":"panic test","action":"eval","code":"x"}`)
	req := testResultReq(true)

	// First task will panic in repo.Create via safeProcessTask.
	r.submit(ctx, rtc, req)
	time.Sleep(50 * time.Millisecond)

	// Second task should still be processed — worker recovered.
	r.submit(ctx, rtc, req)

	deadline := time.After(2 * time.Second)
	for callCount.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timeout: only %d calls processed, want >= 2", callCount.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestRecorder_ConcurrentSubmit(t *testing.T) {
	t.Parallel()

	var processed atomic.Int32
	repo := &mockScriptExecutionRepo{
		createFunc: func(_ context.Context, _ *model.ScriptExecution) error {
			processed.Add(1)
			return nil
		},
	}
	// Use large buffer to ensure all 100 tasks can be queued without dropping.
	deps := &Dependencies{ScriptExecutionRepo: repo}
	r := newScriptExecutionRecorder(deps, 2, 200)
	t.Cleanup(r.shutdown)

	ctx := testContext(t)
	rtc := testRtc(t, `{"title":"concurrent","action":"eval","code":"x"}`)
	req := testResultReq(true)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.submit(ctx, rtc, req)
		}()
	}
	wg.Wait()

	// Wait for all tasks to be processed.
	deadline := time.After(5 * time.Second)
	for processed.Load() < 100 {
		select {
		case <-deadline:
			t.Fatalf("timeout: only %d/100 tasks processed", processed.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mustJSON marshals v to a JSON string literal (for embedding in params JSON).
func mustJSON(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
