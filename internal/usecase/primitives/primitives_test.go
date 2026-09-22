// internal/usecase/primitives/primitives_test.go
package primitives_test

import (
	"context"
	"testing"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

func TestTruncateTitle(t *testing.T) {
	cases := []struct {
		in  string
		max int
		out string
	}{
		{"hello", 10, "hello"},
		{"hello\nworld", 10, "hello"},
		{"日本語テスト", 3, "日本語"},
		{"  trimmed  ", 10, "trimmed"},
	}
	for _, c := range cases {
		if got := primitives.TruncateTitle(c.in, c.max); got != c.out {
			t.Errorf("TruncateTitle(%q, %d) = %q, want %q", c.in, c.max, got, c.out)
		}
	}
}

func TestValidateCreateMessageRequest_Empty(t *testing.T) {
	if err := primitives.ValidateCreateMessageRequest(""); err == nil {
		t.Fatal("expected error for empty content")
	}
}

func TestValidateCreateMessageRequest_OK(t *testing.T) {
	if err := primitives.ValidateCreateMessageRequest("hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckSessionOwnership_SystemAlwaysPass(t *testing.T) {
	// System creator always passes; deps may be nil and it must still not error.
	err := primitives.CheckSessionOwnership(context.Background(), nil, uuid.New(), usecase.SystemCreator{})
	if err != nil {
		t.Fatalf("system creator should always pass, got %v", err)
	}
}

func TestBuildSendMessageUpdates_SkipsForSystemSession(t *testing.T) {
	s := &model.Session{ID: uuid.New(), OwnerKind: string(usecase.CreatorKindSystem), OwnerRefID: "system"}
	turnID := uuid.New()
	if items := primitives.BuildSendMessageUpdates(s, true, &turnID, []uuid.UUID{uuid.New()}); len(items) != 0 {
		t.Fatalf("system session should produce no updates, got %d", len(items))
	}
}

func TestBuildSendMessageUpdates_UserSessionProducesPush(t *testing.T) {
	uid := uuid.New()
	s := &model.Session{ID: uuid.New(), OwnerKind: string(usecase.CreatorKindUser), OwnerRefID: uid.String()}
	turnID := uuid.New()
	items := primitives.BuildSendMessageUpdates(s, true, &turnID, []uuid.UUID{uuid.New()})
	if len(items) != 1 || len(items[0].Items) != 3 {
		t.Fatalf("expected 1 channel with 3 items, got %+v", items)
	}
}

func TestBuildSendMessageUpdates_MultipleMessageIDs(t *testing.T) {
	uid := uuid.New()
	s := &model.Session{ID: uuid.New(), OwnerKind: string(usecase.CreatorKindUser), OwnerRefID: uid.String()}
	turnID := uuid.New()
	msgID1 := uuid.New()
	msgID2 := uuid.New()
	items := primitives.BuildSendMessageUpdates(s, true, &turnID, []uuid.UUID{msgID1, msgID2})
	// Expected: 1 session + 1 turn + 2 messages = 4 items
	if len(items) != 1 || len(items[0].Items) != 4 {
		t.Fatalf("expected 1 channel with 4 items, got %+v", items)
	}
}

// Repository-interaction unit tests should use an in-memory fake implementing
// repo.SessionRepo / TurnRepo / MessageRepo. The full fake is elided here;
// implement it according to the repo interfaces when needed.
func TestCreateSession_PersistsSession(t *testing.T) {
	t.Skip("implement fake repo test when ready")
}

// Placeholder: ensure protocol constants are importable.
var _ = protocol.MessageRoleUser
