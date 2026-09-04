package model_test

import (
	"testing"

	"github.com/rtc-agent/server/internal/model"
)

func TestSession_OwnerFields(t *testing.T) {
	s := model.Session{
		OwnerKind:  "system",
		OwnerRefID: "system",
	}
	if s.OwnerKind != "system" || s.OwnerRefID != "system" {
		t.Fatal("owner fields lost")
	}
}

func TestMessage_CreatorFields(t *testing.T) {
	m := model.Message{
		CreatorKind:  "user",
		CreatorRefID: "00000000-0000-0000-0000-000000000001",
	}
	if m.CreatorKind != "user" {
		t.Fatal("creator kind lost")
	}
}
