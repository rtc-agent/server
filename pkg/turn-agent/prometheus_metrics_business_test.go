package turnagent

import (
	"context"
	"testing"
)

func TestRecordSessionCreated_CounterIncrements(t *testing.T) {
	m := sharedMetrics()
	ctx := context.Background()

	before := getCounterValue(t, m.sessionCreatedTotal)
	m.RecordSessionCreated(ctx)
	after := getCounterValue(t, m.sessionCreatedTotal)

	if after != before+1 {
		t.Errorf("sessionCreatedTotal = %v, want %v", after, before+1)
	}
}

func TestRecordSessionClosed_CounterIncrements(t *testing.T) {
	m := sharedMetrics()
	ctx := context.Background()

	// Test normal close
	normalBefore := getCounterValue(t, m.sessionClosedTotal, "normal")
	m.RecordSessionClosed(ctx, "normal")
	normalAfter := getCounterValue(t, m.sessionClosedTotal, "normal")
	if normalAfter != normalBefore+1 {
		t.Errorf("sessionClosedTotal{normal} = %v, want %v", normalAfter, normalBefore+1)
	}

	// Test error close
	errorBefore := getCounterValue(t, m.sessionClosedTotal, "error")
	m.RecordSessionClosed(ctx, "error")
	errorAfter := getCounterValue(t, m.sessionClosedTotal, "error")
	if errorAfter != errorBefore+1 {
		t.Errorf("sessionClosedTotal{error} = %v, want %v", errorAfter, errorBefore+1)
	}

	// Test empty reason defaults to "unknown"
	unknownBefore := getCounterValue(t, m.sessionClosedTotal, "unknown")
	m.RecordSessionClosed(ctx, "")
	unknownAfter := getCounterValue(t, m.sessionClosedTotal, "unknown")
	if unknownAfter != unknownBefore+1 {
		t.Errorf("sessionClosedTotal{unknown} = %v, want %v", unknownAfter, unknownBefore+1)
	}
}

func TestRecordMessageSent_CounterIncrements(t *testing.T) {
	m := sharedMetrics()
	ctx := context.Background()

	// Test user message
	userBefore := getCounterValue(t, m.messagesSentTotal, "user")
	m.RecordMessageSent(ctx, "user")
	userAfter := getCounterValue(t, m.messagesSentTotal, "user")
	if userAfter != userBefore+1 {
		t.Errorf("messagesSentTotal{user} = %v, want %v", userAfter, userBefore+1)
	}

	// Test assistant message
	assistantBefore := getCounterValue(t, m.messagesSentTotal, "assistant")
	m.RecordMessageSent(ctx, "assistant")
	assistantAfter := getCounterValue(t, m.messagesSentTotal, "assistant")
	if assistantAfter != assistantBefore+1 {
		t.Errorf("messagesSentTotal{assistant} = %v, want %v", assistantAfter, assistantBefore+1)
	}

	// Test system message
	systemBefore := getCounterValue(t, m.messagesSentTotal, "system")
	m.RecordMessageSent(ctx, "system")
	systemAfter := getCounterValue(t, m.messagesSentTotal, "system")
	if systemAfter != systemBefore+1 {
		t.Errorf("messagesSentTotal{system} = %v, want %v", systemAfter, systemBefore+1)
	}

	// Test empty type defaults to "unknown"
	unknownBefore := getCounterValue(t, m.messagesSentTotal, "unknown")
	m.RecordMessageSent(ctx, "")
	unknownAfter := getCounterValue(t, m.messagesSentTotal, "unknown")
	if unknownAfter != unknownBefore+1 {
		t.Errorf("messagesSentTotal{unknown} = %v, want %v", unknownAfter, unknownBefore+1)
	}
}
