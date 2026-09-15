package config

import (
	"strings"
	"testing"
)

func TestWorkerConfig_Validate_ValidConfig(t *testing.T) {
	cfg := &WorkerConfig{
		ContextTokensLimit:      25000,
		AutoCompactBufferTokens: 13000,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config should not return error, got: %v", err)
	}
}

func TestWorkerConfig_Validate_InvalidConfig(t *testing.T) {
	tests := []struct {
		name            string
		contextLimit    int
		compactBuffer   int
		wantErrContains string
	}{
		{
			name:            "buffer greater than limit",
			contextLimit:    100,
			compactBuffer:   200,
			wantErrContains: "worker.context_tokens_limit (100) must be > worker.auto_compact_buffer_tokens (200)",
		},
		{
			name:            "buffer equals limit",
			contextLimit:    1000,
			compactBuffer:   1000,
			wantErrContains: "worker.context_tokens_limit (1000) must be > worker.auto_compact_buffer_tokens (1000)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &WorkerConfig{
				ContextTokensLimit:      tt.contextLimit,
				AutoCompactBufferTokens: tt.compactBuffer,
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected error for invalid config, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}
}

func TestWorkerConfig_Validate_TokenCounterMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{"empty (default)", "", false},
		{"heuristic", "heuristic", false},
		{"tokenizer", "tokenizer", false},
		{"invalid mode", "heurstic", true},
		{"unknown mode", "unknown", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &WorkerConfig{
				TokenCounterMode: tt.mode,
			}
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error for invalid token_counter_mode, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "token_counter_mode") {
				t.Errorf("error = %q, want to contain 'token_counter_mode'", err.Error())
			}
		})
	}
}

func TestWorkerConfig_Validate_ZeroValues(t *testing.T) {
	// When either value is 0, validation should pass (will use defaults elsewhere)
	cfg := &WorkerConfig{
		ContextTokensLimit:      0,
		AutoCompactBufferTokens: 0,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero values should pass validation, got: %v", err)
	}

	// Only compactBuffer is 0
	cfg2 := &WorkerConfig{
		ContextTokensLimit:      25000,
		AutoCompactBufferTokens: 0,
	}
	if err := cfg2.Validate(); err != nil {
		t.Errorf("zero compactBuffer should pass validation, got: %v", err)
	}
}

func TestConfig_Validate_WithInvalidWorkerConfig(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{
			DSN: "postgres://test",
		},
		Server: ServerConfig{
			Port: 8080,
		},
		Auth: AuthConfig{
			JWTSecret:             "test-secret",
			AccessTokenTTLSeconds: 3600,
		},
		LLM: LLMConfig{
			APIKey: "test-key",
		},
		Providers: ProvidersConfig{
			Mock: MockProviderConfig{
				Enabled: true,
			},
		},
		Worker: WorkerConfig{
			ContextTokensLimit:      100,
			AutoCompactBufferTokens: 200,
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error from Config.Validate with invalid WorkerConfig, got nil")
	}
	if !strings.Contains(err.Error(), "context_tokens_limit") {
		t.Errorf("error = %q, want to contain 'context_tokens_limit'", err.Error())
	}
}
