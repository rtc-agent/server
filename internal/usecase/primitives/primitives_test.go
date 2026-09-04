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
	// System Creator 永远放行；deps 传 nil 也不该报错
	err := primitives.CheckSessionOwnership(context.Background(), nil, uuid.New(), usecase.SystemCreator{})
	if err != nil {
		t.Fatalf("system creator should always pass, got %v", err)
	}
}

func TestBuildSendMessageUpdates_SkipsForSystemSession(t *testing.T) {
	s := &model.Session{ID: uuid.New(), OwnerKind: string(usecase.CreatorKindSystem), OwnerRefID: "system"}
	turnID := uuid.New()
	if items := primitives.BuildSendMessageUpdates(s, true, &turnID, uuid.New()); len(items) != 0 {
		t.Fatalf("system session should produce no updates, got %d", len(items))
	}
}

func TestBuildSendMessageUpdates_UserSessionProducesPush(t *testing.T) {
	uid := uuid.New()
	s := &model.Session{ID: uuid.New(), OwnerKind: string(usecase.CreatorKindUser), OwnerRefID: uid.String()}
	turnID := uuid.New()
	items := primitives.BuildSendMessageUpdates(s, true, &turnID, uuid.New())
	if len(items) != 1 || len(items[0].Items) != 3 {
		t.Fatalf("expected 1 channel with 3 items, got %+v", items)
	}
}

// repo 交互的单测建议用 in-memory fake 实现 repo.SessionRepo / TurnRepo / MessageRepo
// 此处省略完整 fake，实现时按 repo 接口定义写一个。
func TestCreateSession_PersistsSession(t *testing.T) {
	t.Skip("实现时补 fake repo 单测")
}

// 占位：确保 protocol 常量可访问
var _ = protocol.MessageRoleUser
