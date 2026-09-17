package agent

import (
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/rtc-agent/server/internal/model"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// Ensure model import is used (TokenUsageUpdate is referenced in eventToTokenUsageUpdate).
var _ = model.TokenUsageUpdate{}

func TestEventToTokenUsageUpdate_Nil(t *testing.T) {
	result := eventToTokenUsageUpdate(nil)
	if result != nil {
		t.Errorf("expected nil for nil input, got %+v", result)
	}
}

func TestEventToTokenUsageUpdate_Zero(t *testing.T) {
	tu := &turnagent.TokenUsage{}
	result := eventToTokenUsageUpdate(tu)
	if result == nil {
		t.Fatal("expected non-nil result for zero usage")
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 || result.TotalTokens != 0 {
		t.Errorf("expected all zeros, got %+v", result)
	}
	if result.CachedTokens != 0 || result.ReasoningTokens != 0 {
		t.Errorf("expected zero detail tokens, got %+v", result)
	}
}

func TestEventToTokenUsageUpdate_Full(t *testing.T) {
	tu := &turnagent.TokenUsage{
		InputTokens:     1000,
		OutputTokens:    500,
		TotalTokens:     1500,
		CachedTokens:    200,
		ReasoningTokens: 100,
	}
	result := eventToTokenUsageUpdate(tu)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000", result.InputTokens)
	}
	if result.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", result.OutputTokens)
	}
	if result.TotalTokens != 1500 {
		t.Errorf("TotalTokens = %d, want 1500", result.TotalTokens)
	}
	if result.CachedTokens != 200 {
		t.Errorf("CachedTokens = %d, want 200", result.CachedTokens)
	}
	if result.ReasoningTokens != 100 {
		t.Errorf("ReasoningTokens = %d, want 100", result.ReasoningTokens)
	}
}

// mergeTokenUsageMax uses model.TokenUsage from eino components/model.
func TestMergeTokenUsageMax_NilSrc(t *testing.T) {
	dst := &einomodel.TokenUsage{PromptTokens: 100}
	mergeTokenUsageMax(dst, nil)
	if dst.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100 (unchanged)", dst.PromptTokens)
	}
}

func TestMergeTokenUsageMax_SrcLarger(t *testing.T) {
	dst := &einomodel.TokenUsage{
		PromptTokens:     50,
		CompletionTokens: 20,
		TotalTokens:      70,
	}
	dst.PromptTokenDetails.CachedTokens = 10
	dst.CompletionTokensDetails.ReasoningTokens = 5

	src := &einomodel.TokenUsage{
		PromptTokens:     100,
		CompletionTokens: 80,
		TotalTokens:      180,
	}
	src.PromptTokenDetails.CachedTokens = 60
	src.CompletionTokensDetails.ReasoningTokens = 25

	mergeTokenUsageMax(dst, src)

	if dst.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", dst.PromptTokens)
	}
	if dst.CompletionTokens != 80 {
		t.Errorf("CompletionTokens = %d, want 80", dst.CompletionTokens)
	}
	if dst.TotalTokens != 180 {
		t.Errorf("TotalTokens = %d, want 180", dst.TotalTokens)
	}
	if dst.PromptTokenDetails.CachedTokens != 60 {
		t.Errorf("CachedTokens = %d, want 60", dst.PromptTokenDetails.CachedTokens)
	}
	if dst.CompletionTokensDetails.ReasoningTokens != 25 {
		t.Errorf("ReasoningTokens = %d, want 25", dst.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestMergeTokenUsageMax_DstLarger(t *testing.T) {
	dst := &einomodel.TokenUsage{
		PromptTokens:     200,
		CompletionTokens: 100,
		TotalTokens:      300,
	}
	dst.PromptTokenDetails.CachedTokens = 80
	dst.CompletionTokensDetails.ReasoningTokens = 40

	src := &einomodel.TokenUsage{
		PromptTokens:     50,
		CompletionTokens: 30,
		TotalTokens:      80,
	}
	src.PromptTokenDetails.CachedTokens = 10
	src.CompletionTokensDetails.ReasoningTokens = 5

	mergeTokenUsageMax(dst, src)

	if dst.PromptTokens != 200 {
		t.Errorf("PromptTokens = %d, want 200", dst.PromptTokens)
	}
	if dst.CompletionTokens != 100 {
		t.Errorf("CompletionTokens = %d, want 100", dst.CompletionTokens)
	}
	if dst.TotalTokens != 300 {
		t.Errorf("TotalTokens = %d, want 300", dst.TotalTokens)
	}
	if dst.PromptTokenDetails.CachedTokens != 80 {
		t.Errorf("CachedTokens = %d, want 80", dst.PromptTokenDetails.CachedTokens)
	}
	if dst.CompletionTokensDetails.ReasoningTokens != 40 {
		t.Errorf("ReasoningTokens = %d, want 40", dst.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestMergeTokenUsageMax_Mixed(t *testing.T) {
	dst := &einomodel.TokenUsage{
		PromptTokens:     200, // dst larger
		CompletionTokens: 10,  // src larger
		TotalTokens:      210, // dst larger
	}
	dst.PromptTokenDetails.CachedTokens = 5          // src larger
	dst.CompletionTokensDetails.ReasoningTokens = 50 // dst larger

	src := &einomodel.TokenUsage{
		PromptTokens:     50,
		CompletionTokens: 80,
		TotalTokens:      130,
	}
	src.PromptTokenDetails.CachedTokens = 30
	src.CompletionTokensDetails.ReasoningTokens = 10

	mergeTokenUsageMax(dst, src)

	if dst.PromptTokens != 200 {
		t.Errorf("PromptTokens = %d, want 200 (dst)", dst.PromptTokens)
	}
	if dst.CompletionTokens != 80 {
		t.Errorf("CompletionTokens = %d, want 80 (src)", dst.CompletionTokens)
	}
	if dst.TotalTokens != 210 {
		t.Errorf("TotalTokens = %d, want 210 (dst)", dst.TotalTokens)
	}
	if dst.PromptTokenDetails.CachedTokens != 30 {
		t.Errorf("CachedTokens = %d, want 30 (src)", dst.PromptTokenDetails.CachedTokens)
	}
	if dst.CompletionTokensDetails.ReasoningTokens != 50 {
		t.Errorf("ReasoningTokens = %d, want 50 (dst)", dst.CompletionTokensDetails.ReasoningTokens)
	}
}
