package updates

import (
	"testing"

	"github.com/rtc-agent/server/pkg/protocol"
)

func TestDerefUpdates_Nil(t *testing.T) {
	result := DerefUpdates(nil)
	if result != nil {
		t.Errorf("DerefUpdates(nil) = %v, want nil", result)
	}
}

func TestDerefUpdates_Empty(t *testing.T) {
	src := []*protocol.Update{}
	result := DerefUpdates(src)
	if result == nil {
		t.Fatal("DerefUpdates(empty) = nil, want non-nil")
	}
	if len(*result) != 0 {
		t.Errorf("len(*result) = %d, want 0", len(*result))
	}
}

func TestDerefUpdates_MultipleItems(t *testing.T) {
	u1 := &protocol.Update{Id: "a"}
	u2 := &protocol.Update{Id: "b"}
	src := []*protocol.Update{u1, u2}
	result := DerefUpdates(src)
	if result == nil {
		t.Fatal("DerefUpdates(src) = nil, want non-nil")
	}
	if len(*result) != 2 {
		t.Fatalf("len(*result) = %d, want 2", len(*result))
	}
	if (*result)[0].Id != "a" {
		t.Errorf("(*result)[0].Id = %q, want %q", (*result)[0].Id, "a")
	}
	if (*result)[1].Id != "b" {
		t.Errorf("(*result)[1].Id = %q, want %q", (*result)[1].Id, "b")
	}
}

func TestDerefUpdates_ReturnsCopies(t *testing.T) {
	// Verify that modifying the result doesn't affect the source
	u1 := &protocol.Update{Id: "original"}
	src := []*protocol.Update{u1}
	result := DerefUpdates(src)

	// Modify the dereferenced copy
	(*result)[0].Id = "modified"

	// Source should be unchanged
	if u1.Id != "original" {
		t.Errorf("source was modified: Id = %q, want %q", u1.Id, "original")
	}
}
