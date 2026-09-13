package model

import (
	"testing"

	"github.com/google/uuid"
)

func TestScriptExecution_BeforeCreate_GeneratesUUID(t *testing.T) {
	t.Parallel()

	s := &ScriptExecution{}
	if s.ID != uuid.Nil {
		t.Fatal("initial ID should be uuid.Nil")
	}

	if err := s.BeforeCreate(nil); err != nil {
		t.Fatalf("BeforeCreate() unexpected error: %v", err)
	}

	if s.ID == uuid.Nil {
		t.Error("expected UUID to be generated, got uuid.Nil")
	}

	// Verify it's a valid UUID v7 (version bits should be 7)
	if v := s.ID.Version(); v != 7 {
		t.Errorf("expected UUID v7, got v%d", v)
	}
}

func TestScriptExecution_BeforeCreate_PreservesExistingID(t *testing.T) {
	t.Parallel()

	existingID, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}

	s := &ScriptExecution{ID: existingID}
	if err := s.BeforeCreate(nil); err != nil {
		t.Fatalf("BeforeCreate() unexpected error: %v", err)
	}

	if s.ID != existingID {
		t.Errorf("BeforeCreate() should not overwrite existing ID: got %s, want %s", s.ID, existingID)
	}
}
