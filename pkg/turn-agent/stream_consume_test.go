package turnagent

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestMergeMaxTokenUsage_NilDst(t *testing.T) {
	src := &schema.TokenUsage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}
	src.PromptTokenDetails.CachedTokens = 30
	src.CompletionTokensDetails.ReasoningTokens = 10

	result := MergeMaxTokenUsage(nil, src)
	if result == nil {
		t.Fatal("expected non-nil result when dst is nil and src is non-nil")
	}
	if result.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", result.PromptTokens)
	}
	if result.CompletionTokens != 50 {
		t.Errorf("CompletionTokens = %d, want 50", result.CompletionTokens)
	}
	if result.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", result.TotalTokens)
	}
	if result.PromptTokenDetails.CachedTokens != 30 {
		t.Errorf("CachedTokens = %d, want 30", result.PromptTokenDetails.CachedTokens)
	}
	if result.CompletionTokensDetails.ReasoningTokens != 10 {
		t.Errorf("ReasoningTokens = %d, want 10", result.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestMergeMaxTokenUsage_NilSrc(t *testing.T) {
	dst := &schema.TokenUsage{PromptTokens: 100}
	result := MergeMaxTokenUsage(dst, nil)
	if result != dst {
		t.Error("expected dst returned when src is nil")
	}
	if result.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", result.PromptTokens)
	}
}

func TestMergeMaxTokenUsage_BothNil(t *testing.T) {
	result := MergeMaxTokenUsage(nil, nil)
	if result != nil {
		t.Error("expected nil when both are nil")
	}
}

func TestMergeMaxTokenUsage_SrcLarger(t *testing.T) {
	dst := &schema.TokenUsage{
		PromptTokens:     50,
		CompletionTokens: 20,
		TotalTokens:      70,
	}
	dst.PromptTokenDetails.CachedTokens = 10
	dst.CompletionTokensDetails.ReasoningTokens = 5

	src := &schema.TokenUsage{
		PromptTokens:     100,
		CompletionTokens: 80,
		TotalTokens:      180,
	}
	src.PromptTokenDetails.CachedTokens = 60
	src.CompletionTokensDetails.ReasoningTokens = 25

	result := MergeMaxTokenUsage(dst, src)
	if result.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", result.PromptTokens)
	}
	if result.CompletionTokens != 80 {
		t.Errorf("CompletionTokens = %d, want 80", result.CompletionTokens)
	}
	if result.TotalTokens != 180 {
		t.Errorf("TotalTokens = %d, want 180", result.TotalTokens)
	}
	if result.PromptTokenDetails.CachedTokens != 60 {
		t.Errorf("CachedTokens = %d, want 60", result.PromptTokenDetails.CachedTokens)
	}
	if result.CompletionTokensDetails.ReasoningTokens != 25 {
		t.Errorf("ReasoningTokens = %d, want 25", result.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestMergeMaxTokenUsage_DstLarger(t *testing.T) {
	dst := &schema.TokenUsage{
		PromptTokens:     200,
		CompletionTokens: 100,
		TotalTokens:      300,
	}
	dst.PromptTokenDetails.CachedTokens = 80
	dst.CompletionTokensDetails.ReasoningTokens = 40

	src := &schema.TokenUsage{
		PromptTokens:     50,
		CompletionTokens: 30,
		TotalTokens:      80,
	}
	src.PromptTokenDetails.CachedTokens = 10
	src.CompletionTokensDetails.ReasoningTokens = 5

	result := MergeMaxTokenUsage(dst, src)
	// dst values should remain unchanged since they are all larger
	if result.PromptTokens != 200 {
		t.Errorf("PromptTokens = %d, want 200", result.PromptTokens)
	}
	if result.CompletionTokens != 100 {
		t.Errorf("CompletionTokens = %d, want 100", result.CompletionTokens)
	}
	if result.TotalTokens != 300 {
		t.Errorf("TotalTokens = %d, want 300", result.TotalTokens)
	}
	if result.PromptTokenDetails.CachedTokens != 80 {
		t.Errorf("CachedTokens = %d, want 80", result.PromptTokenDetails.CachedTokens)
	}
	if result.CompletionTokensDetails.ReasoningTokens != 40 {
		t.Errorf("ReasoningTokens = %d, want 40", result.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestMergeMaxTokenUsage_Mixed(t *testing.T) {
	// Some fields larger in dst, some in src
	dst := &schema.TokenUsage{
		PromptTokens:     200, // dst larger
		CompletionTokens: 10,  // src larger
		TotalTokens:      210, // dst larger
	}
	dst.PromptTokenDetails.CachedTokens = 5          // src larger
	dst.CompletionTokensDetails.ReasoningTokens = 50 // dst larger

	src := &schema.TokenUsage{
		PromptTokens:     50,
		CompletionTokens: 80,
		TotalTokens:      130,
	}
	src.PromptTokenDetails.CachedTokens = 30
	src.CompletionTokensDetails.ReasoningTokens = 10

	result := MergeMaxTokenUsage(dst, src)
	if result.PromptTokens != 200 {
		t.Errorf("PromptTokens = %d, want 200 (dst)", result.PromptTokens)
	}
	if result.CompletionTokens != 80 {
		t.Errorf("CompletionTokens = %d, want 80 (src)", result.CompletionTokens)
	}
	if result.TotalTokens != 210 {
		t.Errorf("TotalTokens = %d, want 210 (dst)", result.TotalTokens)
	}
	if result.PromptTokenDetails.CachedTokens != 30 {
		t.Errorf("CachedTokens = %d, want 30 (src)", result.PromptTokenDetails.CachedTokens)
	}
	if result.CompletionTokensDetails.ReasoningTokens != 50 {
		t.Errorf("ReasoningTokens = %d, want 50 (dst)", result.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestMergeMaxTokenUsage_Idempotent(t *testing.T) {
	// Merging same values should not change anything
	dst := &schema.TokenUsage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}
	dst.PromptTokenDetails.CachedTokens = 30
	dst.CompletionTokensDetails.ReasoningTokens = 10

	src := &schema.TokenUsage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}
	src.PromptTokenDetails.CachedTokens = 30
	src.CompletionTokensDetails.ReasoningTokens = 10

	result := MergeMaxTokenUsage(dst, src)
	if result.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", result.PromptTokens)
	}
	if result.CompletionTokens != 50 {
		t.Errorf("CompletionTokens = %d, want 50", result.CompletionTokens)
	}
}

func TestMergeMaxTokenUsage_MutatesDst(t *testing.T) {
	// MergeMaxTokenUsage mutates dst in place when it's non-nil
	dst := &schema.TokenUsage{PromptTokens: 10}
	src := &schema.TokenUsage{PromptTokens: 100}

	result := MergeMaxTokenUsage(dst, src)
	if result != dst {
		t.Error("expected result to be the same pointer as dst")
	}
	if dst.PromptTokens != 100 {
		t.Errorf("dst.PromptTokens = %d, want 100 (should be mutated in place)", dst.PromptTokens)
	}
}

func TestExtractTokenUsage_Nil(t *testing.T) {
	result := extractTokenUsage(nil)
	if result != nil {
		t.Error("expected nil for nil input")
	}
}

func TestExtractTokenUsage_ZeroUsage(t *testing.T) {
	usage := &schema.TokenUsage{}
	result := extractTokenUsage(usage)
	if result == nil {
		t.Fatal("expected non-nil result for zero usage")
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 || result.TotalTokens != 0 {
		t.Errorf("expected all zeros, got %+v", result)
	}
}

func TestExtractTokenUsage_FullUsage(t *testing.T) {
	usage := &schema.TokenUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}
	usage.PromptTokenDetails.CachedTokens = 200
	usage.CompletionTokensDetails.ReasoningTokens = 100

	result := extractTokenUsage(usage)
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
