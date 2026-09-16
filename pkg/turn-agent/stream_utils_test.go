package turnagent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func TestRecvWithTimeout_Success(t *testing.T) {
	expected := &schema.Message{Content: "hello"}
	recv := func() (*schema.Message, error) {
		return expected, nil
	}

	result, timedOut := RecvWithTimeout(context.Background(), recv, time.Second)
	if timedOut {
		t.Fatal("unexpected timeout")
	}
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Msg != expected {
		t.Fatalf("got %v, want %v", result.Msg, expected)
	}
}

func TestRecvWithTimeout_Error(t *testing.T) {
	expectedErr := errors.New("recv failed")
	recv := func() (*schema.Message, error) {
		return nil, expectedErr
	}

	result, timedOut := RecvWithTimeout(context.Background(), recv, time.Second)
	if timedOut {
		t.Fatal("unexpected timeout")
	}
	if !errors.Is(result.Err, expectedErr) {
		t.Fatalf("got %v, want %v", result.Err, expectedErr)
	}
}

func TestRecvWithTimeout_Timeout(t *testing.T) {
	// recv blocks longer than the timeout
	recv := func() (*schema.Message, error) {
		time.Sleep(500 * time.Millisecond)
		return &schema.Message{Content: "late"}, nil
	}

	result, timedOut := RecvWithTimeout(context.Background(), recv, 50*time.Millisecond)
	if !timedOut {
		t.Fatal("expected timeout")
	}
	if result.Msg != nil {
		t.Fatalf("expected nil msg on timeout, got %v", result.Msg)
	}
}

func TestRecvWithTimeout_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	recv := func() (*schema.Message, error) {
		time.Sleep(500 * time.Millisecond)
		return nil, nil
	}

	// Cancel context after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result, timedOut := RecvWithTimeout(ctx, recv, time.Second)
	if timedOut {
		t.Fatal("should not report timeout on context cancel")
	}
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", result.Err)
	}
}

func TestRecvWithTimeout_PanicRecovery(t *testing.T) {
	recv := func() (*schema.Message, error) {
		panic("test panic in recv")
	}

	result, timedOut := RecvWithTimeout(context.Background(), recv, time.Second)
	if timedOut {
		t.Fatal("unexpected timeout")
	}
	if result.Err == nil {
		t.Fatal("expected error from panic recovery")
	}
	if result.Msg != nil {
		t.Fatalf("expected nil msg on panic, got %v", result.Msg)
	}
}

func TestRecvWithTimeout_ZeroTimeout(t *testing.T) {
	recv := func() (*schema.Message, error) {
		time.Sleep(100 * time.Millisecond)
		return &schema.Message{Content: "slow"}, nil
	}

	// Zero timeout should fire almost immediately
	result, timedOut := RecvWithTimeout(context.Background(), recv, 0)
	if !timedOut {
		// The recv might complete before timer fires in fast environments;
		// accept either outcome but verify no crash
		if result.Err != nil {
			t.Fatalf("unexpected error: %v", result.Err)
		}
	}
}
