package dbmodel_test

import (
	"github.com/rtc-agent/server/internal/dbmodel"
	"testing"
)

func TestSession_OwnerFields(t *testing.T) {
	s := dbmodel.Session{
		OwnerKind:  "system",
		OwnerRefID: "system",
	}
	if s.OwnerKind != "system" || s.OwnerRefID != "system" {
		t.Fatal("owner fields lost")
	}
}

func TestMessage_CreatorFields(t *testing.T) {
	m := dbmodel.Message{
		CreatorKind:  "user",
		CreatorRefID: "00000000-0000-0000-0000-000000000001",
	}
	if m.CreatorKind != "user" {
		t.Fatal("creator kind lost")
	}
}
