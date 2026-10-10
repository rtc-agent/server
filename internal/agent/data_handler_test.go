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

func TestMergeTokenUsageMax(t *testing.T) {
	tests := []struct {
		name     string
		dst      *einomodel.TokenUsage
		src      *einomodel.TokenUsage
		expected *einomodel.TokenUsage
	}{
		{
			name: "SrcLarger",
			dst: &einomodel.TokenUsage{
				PromptTokens:     50,
				CompletionTokens: 20,
				TotalTokens:      70,
			},
			src: &einomodel.TokenUsage{
				PromptTokens:     100,
				CompletionTokens: 80,
				TotalTokens:      180,
			},
			expected: &einomodel.TokenUsage{
				PromptTokens:     100,
				CompletionTokens: 80,
				TotalTokens:      180,
			},
		},
		{
			name: "DstLarger",
			dst: &einomodel.TokenUsage{
				PromptTokens:     200,
				CompletionTokens: 100,
				TotalTokens:      300,
			},
			src: &einomodel.TokenUsage{
				PromptTokens:     50,
				CompletionTokens: 30,
				TotalTokens:      80,
			},
			expected: &einomodel.TokenUsage{
				PromptTokens:     200,
				CompletionTokens: 100,
				TotalTokens:      300,
			},
		},
		{
			name: "Mixed",
			dst: &einomodel.TokenUsage{
				PromptTokens:     200, // dst larger
				CompletionTokens: 10,  // src larger
				TotalTokens:      210, // dst larger
			},
			src: &einomodel.TokenUsage{
				PromptTokens:     50,
				CompletionTokens: 80,
				TotalTokens:      130,
			},
			expected: &einomodel.TokenUsage{
				PromptTokens:     200,
				CompletionTokens: 80,
				TotalTokens:      210,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mergeTokenUsageMax(tt.dst, tt.src)

			if tt.dst.PromptTokens != tt.expected.PromptTokens {
				t.Errorf("PromptTokens = %d, want %d", tt.dst.PromptTokens, tt.expected.PromptTokens)
			}
			if tt.dst.CompletionTokens != tt.expected.CompletionTokens {
				t.Errorf("CompletionTokens = %d, want %d", tt.dst.CompletionTokens, tt.expected.CompletionTokens)
			}
			if tt.dst.TotalTokens != tt.expected.TotalTokens {
				t.Errorf("TotalTokens = %d, want %d", tt.dst.TotalTokens, tt.expected.TotalTokens)
			}
			// Verify detail tokens take the max value
			expectedCached := tt.dst.PromptTokenDetails.CachedTokens
			if tt.src.PromptTokenDetails.CachedTokens > expectedCached {
				expectedCached = tt.src.PromptTokenDetails.CachedTokens
			}
			if tt.dst.PromptTokenDetails.CachedTokens != expectedCached {
				t.Errorf("CachedTokens = %d, want %d", tt.dst.PromptTokenDetails.CachedTokens, expectedCached)
			}

			expectedReasoning := tt.dst.CompletionTokensDetails.ReasoningTokens
			if tt.src.CompletionTokensDetails.ReasoningTokens > expectedReasoning {
				expectedReasoning = tt.src.CompletionTokensDetails.ReasoningTokens
			}
			if tt.dst.CompletionTokensDetails.ReasoningTokens != expectedReasoning {
				t.Errorf("ReasoningTokens = %d, want %d", tt.dst.CompletionTokensDetails.ReasoningTokens, expectedReasoning)
			}
		})
	}
}
