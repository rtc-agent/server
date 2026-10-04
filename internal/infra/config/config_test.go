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
			Env:  "development",
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

func TestEncryptionConfig_Validate(t *testing.T) {
	tests := []struct {
		name            string
		key             string
		wantErr         bool
		wantErrContains string
	}{
		{
			name:            "empty key",
			key:             "",
			wantErr:         true,
			wantErrContains: "session_token_key is required",
		},
		{
			name:            "default test key",
			key:             "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			wantErr:         true,
			wantErrContains: "must not use the default test value",
		},
		{
			name:            "wrong length - too short",
			key:             "0123456789abcdef",
			wantErr:         true,
			wantErrContains: "must be 64 hex characters",
		},
		{
			name:            "wrong length - too long",
			key:             "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef00",
			wantErr:         true,
			wantErrContains: "must be 64 hex characters",
		},
		{
			name:    "valid key",
			key:     "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &EncryptionConfig{
				SessionTokenKey: tt.key,
			}
			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error for invalid config, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
				}
			} else {
				if err != nil {
					t.Errorf("valid config should not return error, got: %v", err)
				}
			}
		})
	}
}

func TestConfig_Validate_EncryptionConfig(t *testing.T) {
	// Test that Config.Validate calls EncryptionConfig.Validate when storage is enabled.
	cfg := &Config{
		Database: DatabaseConfig{
			DSN: "postgres://test",
		},
		Server: ServerConfig{
			Port: 8080,
			Env:  "development",
		},
		LLM: LLMConfig{
			APIKey: "test-key",
		},
		Providers: ProvidersConfig{
			Mock: MockProviderConfig{
				Enabled: true,
			},
		},
		Auth: AuthConfig{
			JWTSecret:             "test-secret",
			AccessTokenTTLSeconds: 3600,
		},
		Storage: StorageConfig{
			Backend: "minio", // Enable storage to trigger encryption validation
			Encryption: EncryptionConfig{
				SessionTokenKey: "", // Invalid: empty
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error from Config.Validate with invalid EncryptionConfig, got nil")
	}
	if !strings.Contains(err.Error(), "session_token_key") {
		t.Errorf("error = %q, want to contain 'session_token_key'", err.Error())
	}

	// Test that encryption validation is skipped when storage is disabled.
	cfg.Storage.Backend = "" // Disable storage
	err = cfg.Validate()
	if err != nil {
		t.Errorf("expected no error when storage is disabled, got: %v", err)
	}
}

func TestTokenExchangeConfig_Validate(t *testing.T) {
	tests := []struct {
		name            string
		config          TokenExchangeConfig
		wantErr         bool
		wantErrContains string
	}{
		{
			name:   "empty config is valid",
			config: TokenExchangeConfig{},
		},
		{
			name: "valid issuer config",
			config: TokenExchangeConfig{
				ExternalIssuers: []ExternalIssuerConfig{
					{
						Name:    "google",
						Issuer:  "https://accounts.google.com",
						JWKSURI: "https://www.googleapis.com/oauth2/v3/certs",
					},
				},
			},
		},
		{
			name: "missing issuer name",
			config: TokenExchangeConfig{
				ExternalIssuers: []ExternalIssuerConfig{
					{
						Issuer:  "https://accounts.google.com",
						JWKSURI: "https://www.googleapis.com/oauth2/v3/certs",
					},
				},
			},
			wantErr:         true,
			wantErrContains: "name is required",
		},
		{
			name: "missing issuer claim",
			config: TokenExchangeConfig{
				ExternalIssuers: []ExternalIssuerConfig{
					{
						Name:    "google",
						JWKSURI: "https://www.googleapis.com/oauth2/v3/certs",
					},
				},
			},
			wantErr:         true,
			wantErrContains: "issuer is required",
		},
		{
			name: "missing jwks_uri",
			config: TokenExchangeConfig{
				ExternalIssuers: []ExternalIssuerConfig{
					{
						Name:   "google",
						Issuer: "https://accounts.google.com",
					},
				},
			},
			wantErr:         true,
			wantErrContains: "jwks_uri is required",
		},
		{
			name: "non-HTTPS jwks_uri rejected",
			config: TokenExchangeConfig{
				ExternalIssuers: []ExternalIssuerConfig{
					{
						Name:    "test",
						Issuer:  "https://test.example.com",
						JWKSURI: "http://test.example.com/jwks",
					},
				},
			},
			wantErr:         true,
			wantErrContains: "must use HTTPS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			}
		})
	}
}
