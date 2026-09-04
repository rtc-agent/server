// internal/usecase/creator_test.go
package usecase

import (
	"testing"

	"github.com/google/uuid"
)

func TestUserCreator_Kind(t *testing.T) {
	uid := uuid.Must(uuid.NewV7())
	c := UserCreator{UserID: uid, DeviceID: "dev-1"}
	if got := c.Kind(); got != CreatorKindUser {
		t.Fatalf("got %q want %q", got, CreatorKindUser)
	}
	if got := c.ReferenceID(); got != uid.String() {
		t.Fatalf("got %q want %q", got, uid.String())
	}
	if c.DeviceID != "dev-1" {
		t.Fatal("DeviceID lost")
	}
}

func TestSystemCreator(t *testing.T) {
	c := SystemCreator{}
	if c.Kind() != CreatorKindSystem {
		t.Fatal("kind")
	}
	if c.ReferenceID() != "system" {
		t.Fatal("ref id")
	}
}
