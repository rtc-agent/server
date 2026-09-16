package agent

import (
	"testing"
)

func TestCalculateCostMicros(t *testing.T) {
	pricing := DefaultModelPricing()

	tests := []struct {
		name     string
		usage    *FullTokenUsage
		expected int64
	}{
		{
			name: "zero usage",
			usage: &FullTokenUsage{
				InputTokens:  0,
				OutputTokens: 0,
			},
			expected: 0,
		},
		{
			name: "input only",
			usage: &FullTokenUsage{
				InputTokens:       1_000_000, // 1M input tokens
				OutputTokens:      0,
				CachedReadTokens:  0,
				CachedWriteTokens: 0,
				ReasoningTokens:   0,
			},
			// 1M * $3/M = $3 = 3_000_000 micros
			expected: 3_000_000,
		},
		{
			name: "output only",
			usage: &FullTokenUsage{
				InputTokens:  0,
				OutputTokens: 1_000_000, // 1M output tokens
			},
			// 1M * $15/M = $15 = 15_000_000 micros
			expected: 15_000_000,
		},
		{
			name: "cached read",
			usage: &FullTokenUsage{
				InputTokens:      0,
				CachedReadTokens: 1_000_000, // 1M cached read
			},
			// 1M * $0.3/M = $0.3 = 300_000 micros
			expected: 300_000,
		},
		{
			name: "cached write",
			usage: &FullTokenUsage{
				InputTokens:       0,
				CachedWriteTokens: 1_000_000, // 1M cached write
			},
			// 1M * $3.75/M = $3.75 = 3_750_000 micros
			expected: 3_750_000,
		},
		{
			name: "mixed usage",
			usage: &FullTokenUsage{
				InputTokens:       100_000,
				OutputTokens:      50_000,
				CachedReadTokens:  200_000,
				CachedWriteTokens: 25_000,
				ReasoningTokens:   10_000,
			},
			// Input: 100K * $3/M = $0.3
			// CachedRead: 200K * $0.3/M = $0.06
			// CachedWrite: 25K * $3.75/M = $0.09375
			// Output: 50K * $15/M = $0.75
			// Reasoning: 10K * $0/M = $0
			// Total = $1.20375 = 1_203_750 micros
			expected: 1_203_750,
		},
		{
			name: "nil usage fields default to zero",
			usage: &FullTokenUsage{
				TotalTokens: 500_000,
			},
			// All token dimension fields are 0, so cost is 0 regardless of TotalTokens.
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateCostMicros(tt.usage, pricing)
			if result != tt.expected {
				t.Errorf("calculateCostMicros() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestDefaultModelPricing(t *testing.T) {
	p := DefaultModelPricing()
	if p.InputPerMillion != 3.0 {
		t.Errorf("InputPerMillion = %f, want 3.0", p.InputPerMillion)
	}
	if p.OutputPerMillion != 15.0 {
		t.Errorf("OutputPerMillion = %f, want 15.0", p.OutputPerMillion)
	}
	if p.CachedReadPerMillion != 0.3 {
		t.Errorf("CachedReadPerMillion = %f, want 0.3", p.CachedReadPerMillion)
	}
	if p.CachedWritePerMillion != 3.75 {
		t.Errorf("CachedWritePerMillion = %f, want 3.75", p.CachedWritePerMillion)
	}
	if p.ReasoningPerMillion != 0.0 {
		t.Errorf("ReasoningPerMillion = %f, want 0.0", p.ReasoningPerMillion)
	}
}

func TestNewModelPricing_NilConfig(t *testing.T) {
	p := NewModelPricing(nil)
	d := DefaultModelPricing()
	if p != d {
		t.Errorf("NewModelPricing(nil) should return DefaultModelPricing()")
	}
}

func TestNewModelPricing_CustomConfig(t *testing.T) {
	cfg := &ModelPricingConfig{
		InputPerMillion:       10.0,
		OutputPerMillion:      30.0,
		CachedReadPerMillion:  1.0,
		CachedWritePerMillion: 12.5,
		ReasoningPerMillion:   5.0,
	}
	p := NewModelPricing(cfg)
	if p.InputPerMillion != 10.0 {
		t.Errorf("InputPerMillion = %f, want 10.0", p.InputPerMillion)
	}
	if p.OutputPerMillion != 30.0 {
		t.Errorf("OutputPerMillion = %f, want 30.0", p.OutputPerMillion)
	}
}
