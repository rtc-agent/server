package updates

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/centrifugal/centrifuge"
	"github.com/rtc-agent/server/pkg/protocol"
)

func TestFillGapPublications_NoGaps(t *testing.T) {
	// Continuous offsets [3,4,5] with sinceOffset=2 and latestOffset=5
	pubs := []*centrifuge.Publication{
		{Offset: 3, Data: []byte(`"a"`)},
		{Offset: 4, Data: []byte(`"b"`)},
		{Offset: 5, Data: []byte(`"c"`)},
	}
	result := fillGapPublications(context.Background(), 2, 5, pubs)
	if len(result) != 3 {
		t.Fatalf("expected 3 publications, got %d", len(result))
	}
	for i, p := range result {
		if p.Offset != uint64(3+i) {
			t.Errorf("result[%d].Offset = %d, want %d", i, p.Offset, 3+i)
		}
	}
}

func TestFillGapPublications_LeadingGap(t *testing.T) {
	// sinceOffset=2, pubs start at offset 5, latestOffset=5
	// Missing offsets: 3, 4
	pubs := []*centrifuge.Publication{
		{Offset: 5, Data: []byte(`"a"`)},
	}
	result := fillGapPublications(context.Background(), 2, 5, pubs)
	// Expected: gap(3), gap(4), pub(5)
	if len(result) != 3 {
		t.Fatalf("expected 3 publications, got %d", len(result))
	}
	// Check gap publications
	for i := 0; i < 2; i++ {
		if result[i].Offset != uint64(3+i) {
			t.Errorf("result[%d].Offset = %d, want %d", i, result[i].Offset, 3+i)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(result[i].Data, &payload); err != nil {
			t.Fatalf("result[%d] data is not valid JSON: %v", i, err)
		}
		if string(payload["type"]) != `"gap"` {
			t.Errorf("result[%d] type = %s, want gap", i, string(payload["type"]))
		}
	}
	// Check real publication
	if result[2].Offset != 5 {
		t.Errorf("result[2].Offset = %d, want 5", result[2].Offset)
	}
}

func TestFillGapPublications_TrailingGap(t *testing.T) {
	// sinceOffset=2, pub at offset 3, latestOffset=5
	// Missing offsets: 4, 5
	pubs := []*centrifuge.Publication{
		{Offset: 3, Data: []byte(`"a"`)},
	}
	result := fillGapPublications(context.Background(), 2, 5, pubs)
	// Expected: pub(3), gap(4), gap(5)
	if len(result) != 3 {
		t.Fatalf("expected 3 publications, got %d", len(result))
	}
	if result[0].Offset != 3 {
		t.Errorf("result[0].Offset = %d, want 3", result[0].Offset)
	}
	for i := 1; i < 3; i++ {
		if result[i].Offset != uint64(3+i) {
			t.Errorf("result[%d].Offset = %d, want %d", i, result[i].Offset, 3+i)
		}
	}
}

func TestFillGapPublications_MiddleGap(t *testing.T) {
	// sinceOffset=2, pubs at offsets 3 and 6, latestOffset=6
	// Missing offsets: 4, 5
	pubs := []*centrifuge.Publication{
		{Offset: 3, Data: []byte(`"a"`)},
		{Offset: 6, Data: []byte(`"b"`)},
	}
	result := fillGapPublications(context.Background(), 2, 6, pubs)
	// Expected: pub(3), gap(4), gap(5), pub(6)
	if len(result) != 4 {
		t.Fatalf("expected 4 publications, got %d", len(result))
	}
	if result[0].Offset != 3 {
		t.Errorf("result[0].Offset = %d, want 3", result[0].Offset)
	}
	if result[1].Offset != 4 {
		t.Errorf("result[1].Offset = %d, want 4", result[1].Offset)
	}
	if result[2].Offset != 5 {
		t.Errorf("result[2].Offset = %d, want 5", result[2].Offset)
	}
	if result[3].Offset != 6 {
		t.Errorf("result[3].Offset = %d, want 6", result[3].Offset)
	}
}

func TestFillGapPublications_EmptyWithGap(t *testing.T) {
	// sinceOffset=2, no pubs, latestOffset=5
	// All offsets 3,4,5 are gaps
	result := fillGapPublications(context.Background(), 2, 5, nil)
	if len(result) != 3 {
		t.Fatalf("expected 3 gap publications, got %d", len(result))
	}
	for i, p := range result {
		if p.Offset != uint64(3+i) {
			t.Errorf("result[%d].Offset = %d, want %d", i, p.Offset, 3+i)
		}
	}
}

func TestFillGapPublications_EmptyNoGap(t *testing.T) {
	// sinceOffset == latestOffset, no pubs
	result := fillGapPublications(context.Background(), 5, 5, nil)
	if len(result) != 0 {
		t.Fatalf("expected 0 publications, got %d", len(result))
	}
}

func TestFillGapPublications_SinglePublication(t *testing.T) {
	// sinceOffset=0, single pub at offset 1, latestOffset=1
	pubs := []*centrifuge.Publication{
		{Offset: 1, Data: []byte(`"a"`)},
	}
	result := fillGapPublications(context.Background(), 0, 1, pubs)
	if len(result) != 1 {
		t.Fatalf("expected 1 publication, got %d", len(result))
	}
	if result[0].Offset != 1 {
		t.Errorf("result[0].Offset = %d, want 1", result[0].Offset)
	}
}

func TestMakeGapPublication_Structure(t *testing.T) {
	pub := makeGapPublication(context.Background(), 42)
	if pub.Offset != 42 {
		t.Errorf("Offset = %d, want 42", pub.Offset)
	}

	var payload struct {
		Type string                 `json:"type"`
		Data protocol.UpdateDataGap `json:"data"`
	}
	if err := json.Unmarshal(pub.Data, &payload); err != nil {
		t.Fatalf("failed to unmarshal gap data: %v", err)
	}
	if payload.Type != string(protocol.UpdateTypeGap) {
		t.Errorf("Type = %q, want %q", payload.Type, protocol.UpdateTypeGap)
	}
}
