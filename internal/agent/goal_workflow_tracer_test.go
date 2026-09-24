package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/usecase"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestGoalWorkflow_OnTurnComplete_NilTracer verifies that GoalWorkflow.OnTurnComplete
// doesn't panic when helpers.tracer is nil (defaults to noop tracer).
// This test catches the production panic we fixed.
func TestGoalWorkflow_OnTurnComplete_NilTracer(t *testing.T) {
	// Create helpers with nil tracer (simulating missing Config.Tracer)
	h := &helpers{
		deps:   &usecase.Dependencies{},
		tracer: nil, // This should be defaulted to noop tracer
	}
	applyHelperDefaults(h)

	// Verify tracer was defaulted
	if h.tracer == nil {
		t.Fatal("applyHelperDefaults should set a default noop tracer when tracer is nil")
	}

	// Create GoalWorkflow with the helpers
	registry := command.NewCommandRegistry()
	gw := &GoalWorkflow{helpers: h, registry: registry}
	registry.Register(gw)

	// Create a test context
	sessionID := uuid.New()
	turnID := uuid.New()
	ctx := command.Context{
		Context:   context.Background(),
		SessionID: sessionID,
		TurnID:    turnID,
	}

	// This should not panic (the bug we fixed)
	err := gw.OnTurnComplete(ctx)
	if err != nil {
		t.Errorf("OnTurnComplete returned error: %v", err)
	}
}

// TestLoopWorkflow_OnTurnComplete_NilTracer verifies that LoopWorkflow.OnTurnComplete
// doesn't panic when helpers.tracer is nil (defaults to noop tracer).
func TestLoopWorkflow_OnTurnComplete_NilTracer(t *testing.T) {
	// Create helpers with nil tracer
	h := &helpers{
		deps:   &usecase.Dependencies{},
		tracer: nil,
	}
	applyHelperDefaults(h)

	// Verify tracer was defaulted
	if h.tracer == nil {
		t.Fatal("applyHelperDefaults should set a default noop tracer when tracer is nil")
	}

	// Create LoopWorkflow with the helpers
	registry := command.NewCommandRegistry()
	lw := &LoopWorkflow{helpers: h, registry: registry}
	registry.Register(lw)

	// Create a test context
	sessionID := uuid.New()
	turnID := uuid.New()
	ctx := command.Context{
		Context:   context.Background(),
		SessionID: sessionID,
		TurnID:    turnID,
	}

	// This should not panic
	err := lw.OnTurnComplete(ctx)
	if err != nil {
		t.Errorf("OnTurnComplete returned error: %v", err)
	}
}

// TestApplyHelperDefaults_NoopTracer verifies that applyHelperDefaults sets
// a noop tracer when tracer is nil.
func TestApplyHelperDefaults_NoopTracer(t *testing.T) {
	h := &helpers{
		tracer: nil,
	}
	applyHelperDefaults(h)

	if h.tracer == nil {
		t.Fatal("applyHelperDefaults should set a default tracer")
	}

	// Verify it's a noop tracer by calling Start (should not panic)
	ctx := context.Background()
	_, span := h.tracer.Start(ctx, "test-span")
	if span == nil {
		t.Error("tracer.Start should return a non-nil span")
	}
	span.End()
}

// TestApplyHelperDefaults_PreservesExistingTracer verifies that applyHelperDefaults
// doesn't overwrite an existing tracer.
func TestApplyHelperDefaults_PreservesExistingTracer(t *testing.T) {
	existingTracer := noop.NewTracerProvider().Tracer("existing")
	h := &helpers{
		tracer: existingTracer,
	}
	applyHelperDefaults(h)

	if h.tracer != existingTracer {
		t.Error("applyHelperDefaults should preserve existing tracer")
	}
}
