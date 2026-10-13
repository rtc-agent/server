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
		Redis: RedisConfig{
			Addr: "localhost:6379",
		},
		Auth: AuthConfig{
			JWTSecret:             "test-secret-key-32bytes-long!!!!",
			AccessTokenTTLSeconds: 3600,
		},
		LLM: LLMConfig{
			APIKey: "test-key",
		},
		Providers: ProvidersConfig{
			Mock: MockProviderConfig{
				Enabled:      true,
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
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
		Redis: RedisConfig{
			Addr: "localhost:6379",
		},
		LLM: LLMConfig{
			APIKey: "test-key",
		},
		Providers: ProvidersConfig{
			Mock: MockProviderConfig{
				Enabled:      true,
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
			},
		},
		Auth: AuthConfig{
			JWTSecret:             "test-secret-key-32bytes-long!!!!",
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
		serverEnv       string
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
			serverEnv: "development",
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
		{
			name: "production requires expected_audience",
			config: TokenExchangeConfig{
				ExternalIssuers: []ExternalIssuerConfig{
					{
						Name:    "google",
						Issuer:  "https://accounts.google.com",
						JWKSURI: "https://www.googleapis.com/oauth2/v3/certs",
						// ExpectedAudience intentionally empty
					},
				},
			},
			serverEnv:       "production",
			wantErr:         true,
			wantErrContains: "expected_audience is required in production",
		},
		{
			name: "production with expected_audience is valid",
			config: TokenExchangeConfig{
				ExternalIssuers: []ExternalIssuerConfig{
					{
						Name:             "google",
						Issuer:           "https://accounts.google.com",
						JWKSURI:          "https://www.googleapis.com/oauth2/v3/certs",
						ExpectedAudience: "my-rtc-app-client-id",
					},
				},
			},
			serverEnv: "production",
			wantErr:   false,
		},
		{
			name: "development without expected_audience is valid",
			config: TokenExchangeConfig{
				ExternalIssuers: []ExternalIssuerConfig{
					{
						Name:    "google",
						Issuer:  "https://accounts.google.com",
						JWKSURI: "https://www.googleapis.com/oauth2/v3/certs",
					},
				},
			},
			serverEnv: "development",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate(tt.serverEnv)
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

func TestConfig_IsDevBypassAllowed(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		expected bool
	}{
		{
			name:     "development environment allows bypass",
			env:      "development",
			expected: true,
		},
		{
			name:     "dev shorthand allows bypass",
			env:      "dev",
			expected: true,
		},
		{
			name:     "production environment blocks bypass",
			env:      "production",
			expected: false,
		},
		{
			name:     "prod shorthand blocks bypass",
			env:      "prod",
			expected: false,
		},
		{
			name:     "staging environment blocks bypass",
			env:      "staging",
			expected: false,
		},
		{
			name:     "unknown environment blocks bypass (secure default)",
			env:      "test",
			expected: false,
		},
		{
			name:     "empty environment blocks bypass (secure default)",
			env:      "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Server: ServerConfig{
					Env: tt.env,
				},
			}
			result := cfg.IsDevBypassAllowed()
			if result != tt.expected {
				t.Errorf("IsDevBypassAllowed() with env=%q = %v, want %v", tt.env, result, tt.expected)
			}
		})
	}
}

func TestConfig_ValidateDevBypassSafety(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		wantWarning bool
	}{
		{
			name:        "development environment - no warning",
			env:         "development",
			wantWarning: false,
		},
		{
			name:        "production environment - no warning (bypass correctly blocked)",
			env:         "production",
			wantWarning: false,
		},
		{
			name:        "staging environment - no warning (bypass correctly blocked)",
			env:         "staging",
			wantWarning: false,
		},
		{
			name:        "unknown environment - no warning (bypass correctly blocked by secure default)",
			env:         "unknown",
			wantWarning: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Server: ServerConfig{
					Env: tt.env,
				},
			}
			warning := cfg.ValidateDevBypassSafety()
			hasWarning := warning != ""
			if hasWarning != tt.wantWarning {
				t.Errorf("ValidateDevBypassSafety() with env=%q: got warning=%v, want warning=%v",
					tt.env, hasWarning, tt.wantWarning)
			}
		})
	}
}

func TestConfig_ValidateCORS(t *testing.T) {
	// newMinimalValidConfig creates a config with minimal valid settings
	// to isolate CORS validation from other validation rules.
	newMinimalValidConfig := func(env string, allowOrigins []string) *Config {
		cfg := &Config{
			Database: DatabaseConfig{
				DSN: "postgres://test",
			},
			Server: ServerConfig{
				Port: 8080,
				Env:  env,
			},
			Redis: RedisConfig{
				Addr: "localhost:6379",
			},
			Auth: AuthConfig{
				JWTSecret:             "test-secret-key-32bytes-long!!!!",
				AccessTokenTTLSeconds: 3600,
				AllowedRedirectURIs:   []string{"https://app.example.com/callback"}, // Required for production
			},
			LLM: LLMConfig{
				APIKey: "test-key",
			},
			Providers: ProvidersConfig{
				Mock: MockProviderConfig{
					Enabled:      env == "development", // Mock provider only allowed in development
					ClientID:     "test-client-id",
					ClientSecret: "test-client-secret",
				},
				GitHub: GitHubProviderConfig{
					Enabled:      env == "production", // GitHub provider for production
					ClientID:     "test-github-client-id",
					ClientSecret: "test-github-client-secret",
				},
			},
			CORS: CORSConfig{
				AllowOrigins: allowOrigins,
			},
		}
		// Add Redis password for production to avoid warnings
		if env == "production" {
			cfg.Redis.Password = "test-password"
		}
		return cfg
	}

	tests := []struct {
		name            string
		env             string
		allowOrigins    []string
		wantErr         bool
		wantErrContains string
	}{
		{
			name:         "development - localhost wildcard allowed",
			env:          "development",
			allowOrigins: []string{"http://localhost:*"},
			wantErr:      false,
		},
		{
			name:         "development - wildcard * allowed",
			env:          "development",
			allowOrigins: []string{"*"},
			wantErr:      false,
		},
		{
			name:            "production - localhost wildcard blocked",
			env:             "production",
			allowOrigins:    []string{"http://localhost:*"},
			wantErr:         true,
			wantErrContains: "localhost:*",
		},
		{
			name:            "production - wildcard * blocked",
			env:             "production",
			allowOrigins:    []string{"*"},
			wantErr:         true,
			wantErrContains: "*",
		},
		{
			name:         "production - explicit origins allowed",
			env:          "production",
			allowOrigins: []string{"https://app.example.com", "https://admin.example.com"},
			wantErr:      false,
		},
		{
			name:         "production - explicit localhost port allowed",
			env:          "production",
			allowOrigins: []string{"http://localhost:3000"},
			wantErr:      false,
		},
		{
			name:         "production - empty origins allowed with warning",
			env:          "production",
			allowOrigins: []string{},
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newMinimalValidConfig(tt.env, tt.allowOrigins)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() with env=%q, origins=%v: got err=%v, wantErr=%v",
					tt.env, tt.allowOrigins, err, tt.wantErr)
			}
			if tt.wantErrContains != "" && err != nil {
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("Validate() error = %q, want to contain %q", err.Error(), tt.wantErrContains)
				}
			}
		})
	}
}

func TestValidateJSON(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid object",
			value:   map[string]any{"key": "value"},
			wantErr: false,
		},
		{
			name:    "valid array",
			value:   []any{"item1", "item2"},
			wantErr: false,
		},
		{
			name:    "empty object",
			value:   map[string]any{},
			wantErr: false,
		},
		{
			name:    "empty array",
			value:   []any{},
			wantErr: false,
		},
		{
			name:    "nested object",
			value:   map[string]any{"nested": map[string]any{"key": "value"}},
			wantErr: false,
		},
		{
			name:    "nested array",
			value:   []any{[]any{1, 2}, []any{3, 4}},
			wantErr: false,
		},
		{
			name:    "null value",
			value:   nil,
			wantErr: true,
			errMsg:  "cannot be null",
		},
		{
			name:    "string primitive",
			value:   "not a json structure",
			wantErr: true,
			errMsg:  "must be an object or array",
		},
		{
			name:    "number primitive",
			value:   123,
			wantErr: true,
			errMsg:  "must be an object or array",
		},
		{
			name:    "float primitive",
			value:   3.14,
			wantErr: true,
			errMsg:  "must be an object or array",
		},
		{
			name:    "bool primitive",
			value:   true,
			wantErr: true,
			errMsg:  "must be an object or array",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateJSON(tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateJSON() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errMsg != "" && err != nil {
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateJSON() error = %q, want to contain %q", err.Error(), tt.errMsg)
				}
			}
		})
	}
}
