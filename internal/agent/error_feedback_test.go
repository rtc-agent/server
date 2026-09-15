package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// ---------------------------------------------------------------------------
// classifyError tests
// ---------------------------------------------------------------------------

func TestClassifyError_PromptTooLong(t *testing.T) {
	err := errors.New("prompt is too long; reduce your prompt")
	cat, _, _, retryable := classifyError(err)
	if cat != protocol.ErrorCategoryContext {
		t.Errorf("category = %q, want %q", cat, protocol.ErrorCategoryContext)
	}
	if !retryable {
		t.Error("expected retryable=true for prompt-too-long")
	}
}

func TestClassifyError_StreamIdleTimeout(t *testing.T) {
	err := &turnagent.StreamIdleTimeoutError{
		SessionID: "s1",
		TurnID:    "t1",
		Timeout:   3 * time.Minute,
	}
	cat, _, _, retryable := classifyError(err)
	if cat != protocol.ErrorCategoryTimeout {
		t.Errorf("category = %q, want %q", cat, protocol.ErrorCategoryTimeout)
	}
	if retryable {
		t.Error("expected retryable=false for stream idle timeout")
	}
}

func TestClassifyError_StreamIdleTimeout_Wrapped(t *testing.T) {
	inner := &turnagent.StreamIdleTimeoutError{
		SessionID: "s1",
		TurnID:    "t1",
		Timeout:   3 * time.Minute,
	}
	err := fmt.Errorf("consume stream: %w", inner)
	cat, _, _, retryable := classifyError(err)
	if cat != protocol.ErrorCategoryTimeout {
		t.Errorf("wrapped: category = %q, want %q", cat, protocol.ErrorCategoryTimeout)
	}
	if retryable {
		t.Error("wrapped: expected retryable=false")
	}
}

func TestClassifyError_ContextCanceled(t *testing.T) {
	// context.Canceled should fall through to system (default).
	// The caller (failTurn callback) uses ShouldSkipErrorMessage to avoid
	// inserting an error message, but classifyError itself still returns system.
	cat, _, _, retryable := classifyError(context.Canceled)
	if cat != protocol.ErrorCategorySystem {
		t.Errorf("category = %q, want %q", cat, protocol.ErrorCategorySystem)
	}
	if retryable {
		t.Error("expected retryable=false for context.Canceled")
	}
}

func TestClassifyError_ContextDeadlineExceeded(t *testing.T) {
	cat, _, _, retryable := classifyError(context.DeadlineExceeded)
	if cat != protocol.ErrorCategoryTimeout {
		t.Errorf("category = %q, want %q", cat, protocol.ErrorCategoryTimeout)
	}
	if retryable {
		t.Error("expected retryable=false for DeadlineExceeded")
	}
}

func TestClassifyError_UnknownError(t *testing.T) {
	err := errors.New("something weird happened")
	cat, _, _, retryable := classifyError(err)
	if cat != protocol.ErrorCategorySystem {
		t.Errorf("category = %q, want %q", cat, protocol.ErrorCategorySystem)
	}
	if retryable {
		t.Error("expected retryable=false for unknown error")
	}
}

func TestClassifyError_NilError(t *testing.T) {
	cat, _, _, retryable := classifyError(nil)
	if cat != protocol.ErrorCategorySystem {
		t.Errorf("category = %q, want %q", cat, protocol.ErrorCategorySystem)
	}
	if retryable {
		t.Error("expected retryable=false for nil error")
	}
}

func TestClassifyError_NetworkError(t *testing.T) {
	// net.OpError -> network
	err := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	cat, _, _, retryable := classifyError(err)
	if cat != protocol.ErrorCategoryNetwork {
		t.Errorf("net.OpError: category = %q, want %q", cat, protocol.ErrorCategoryNetwork)
	}
	if !retryable {
		t.Error("net.OpError: expected retryable=true")
	}
}

func TestClassifyError_URLError(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://api.example.com", Err: errors.New("timeout")}
	cat, _, _, retryable := classifyError(err)
	if cat != protocol.ErrorCategoryNetwork {
		t.Errorf("url.Error: category = %q, want %q", cat, protocol.ErrorCategoryNetwork)
	}
	if !retryable {
		t.Error("url.Error: expected retryable=true")
	}
}

func TestClassifyError_AnthropicAPIError(t *testing.T) {
	// Build an anthropic.Error via JSON unmarshaling (fields are unexported).
	tests := []struct {
		name         string
		errorType    string
		wantCategory protocol.ErrorCategory
		wantRetry    bool
	}{
		{"overloaded", "overloaded_error", protocol.ErrorCategoryAPI, true},
		{"rate_limit", "rate_limit_error", protocol.ErrorCategoryAPI, true},
		{"authentication", "authentication_error", protocol.ErrorCategoryPermission, false},
		{"permission", "permission_error", protocol.ErrorCategoryPermission, false},
		{"invalid_request", "invalid_request_error", protocol.ErrorCategoryContext, false},
		{"timeout", "timeout_error", protocol.ErrorCategoryTimeout, true},
		{"unknown_type", "some_new_error_type", protocol.ErrorCategoryAPI, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			apiErr := buildAnthropicError(t, tc.errorType)
			cat, _, _, retryable := classifyError(apiErr)
			if cat != tc.wantCategory {
				t.Errorf("category = %q, want %q", cat, tc.wantCategory)
			}
			if retryable != tc.wantRetry {
				t.Errorf("retryable = %v, want %v", retryable, tc.wantRetry)
			}
		})
	}
}

// buildAnthropicError constructs an *anthropic.Error by unmarshaling JSON.
// Request and Response must be populated because Error() accesses them.
func buildAnthropicError(t *testing.T, errorType string) *anthropic.Error {
	t.Helper()
	payload := fmt.Sprintf(`{"error":{"type":%q,"message":"test error"}}`, errorType)
	var apiErr anthropic.Error
	if err := json.Unmarshal([]byte(payload), &apiErr); err != nil {
		t.Fatalf("failed to build anthropic.Error: %v", err)
	}
	// Error() reads Request.Method, Request.URL, Response.StatusCode.
	apiErr.Request = &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Scheme: "https", Host: "api.anthropic.com", Path: "/v1/messages"},
	}
	apiErr.Response = &http.Response{
		StatusCode: http.StatusBadRequest,
	}
	apiErr.StatusCode = http.StatusBadRequest
	return &apiErr
}

// ---------------------------------------------------------------------------
// sanitizeRawError tests
// ---------------------------------------------------------------------------

func TestSanitizeRawError_APIKeyRedaction(t *testing.T) {
	input := "request failed with key sk-ant-abc123XYZ456def leaked"
	got := sanitizeRawError(input)
	if strings.Contains(got, "sk-ant-") {
		t.Errorf("API key not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED_API_KEY]") {
		t.Errorf("expected [REDACTED_API_KEY] placeholder: %s", got)
	}
}

func TestSanitizeRawError_BearerTokenRedaction(t *testing.T) {
	input := "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.test.sig_value"
	got := sanitizeRawError(input)
	if strings.Contains(got, "eyJhbGci") {
		t.Errorf("Bearer token not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED_TOKEN]") {
		t.Errorf("expected [REDACTED_TOKEN] placeholder: %s", got)
	}
}

func TestSanitizeRawError_InternalIP(t *testing.T) {
	input := "connection refused to 10.0.1.5:6379"
	got := sanitizeRawError(input)
	if strings.Contains(got, "10.0.1.5") {
		t.Errorf("internal IP not redacted: %s", got)
	}
	if !strings.Contains(got, "[INTERNAL_ADDR]") {
		t.Errorf("expected [INTERNAL_ADDR] placeholder: %s", got)
	}
}

func TestSanitizeRawError_InternalIP_PrivateRanges(t *testing.T) {
	tests := []string{
		"10.0.0.1",
		"172.16.0.1",
		"172.31.255.255",
		"192.168.1.1",
		"myhost.internal",
	}
	for _, ip := range tests {
		t.Run(ip, func(t *testing.T) {
			input := "error at " + ip
			got := sanitizeRawError(input)
			if strings.Contains(got, ip) {
				t.Errorf("address %q not redacted: %s", ip, got)
			}
		})
	}
}

func TestSanitizeRawError_Truncation(t *testing.T) {
	// Build a string longer than maxRawErrorLen (500).
	input := strings.Repeat("x", 600)
	got := sanitizeRawError(input)
	// Expect output to be maxRawErrorLen + len("...")
	expectedLen := maxRawErrorLen + len("...")
	if len(got) != expectedLen {
		t.Errorf("truncated length = %d, want %d", len(got), expectedLen)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected truncation suffix '...': %s", got[len(got)-20:])
	}
}

func TestSanitizeRawError_EmailRedaction(t *testing.T) {
	input := "contact admin@example.com for help"
	got := sanitizeRawError(input)
	if strings.Contains(got, "admin@example.com") {
		t.Errorf("email not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Errorf("expected [REDACTED_EMAIL]: %s", got)
	}
}

func TestSanitizeRawError_AWSKeyRedaction(t *testing.T) {
	input := "aws key AKIAIOSFODNN7EXAMPLE"
	got := sanitizeRawError(input)
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS key not redacted: %s", got)
	}
}

func TestSanitizeRawError_VCSTokenRedaction(t *testing.T) {
	tests := []struct {
		token string
	}{
		{"ghp_abcdefghijklmnopqrstuvwxyz1234567890"},
		{"glpat-abcdefghijklmnopqrstuvwx"},
		{"gho_abcdefghijklmnopqrstuvwxyz123456"},
	}
	for _, tc := range tests {
		t.Run(tc.token[:8], func(t *testing.T) {
			input := "token=" + tc.token
			got := sanitizeRawError(input)
			if strings.Contains(got, tc.token) {
				t.Errorf("VCS token not redacted: %s", got)
			}
		})
	}
}

func TestSanitizeRawError_FilePathUsername(t *testing.T) {
	input := "file at /home/alice/secret/data.txt not found"
	got := sanitizeRawError(input)
	if strings.Contains(got, "/home/alice/") {
		t.Errorf("username not redacted: %s", got)
	}
	if !strings.Contains(got, "[USER]") {
		t.Errorf("expected [USER] placeholder: %s", got)
	}
}

func TestSanitizeRawError_EmptyInput(t *testing.T) {
	if got := sanitizeRawError(""); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestSanitizeRawError_NoSensitiveData(t *testing.T) {
	input := "simple error message without sensitive data"
	if got := sanitizeRawError(input); got != input {
		t.Errorf("expected unchanged string, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// 补充的净化规则测试
// ---------------------------------------------------------------------------

func TestSanitizeRawError_CredentialURIRedaction(t *testing.T) {
	input := "connected to redis://admin:secretpass@localhost:6379"
	got := sanitizeRawError(input)
	if strings.Contains(got, "admin:secretpass") {
		t.Errorf("credential URI not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED_URI]") {
		t.Errorf("expected [REDACTED_URI] placeholder: %s", got)
	}
}

func TestSanitizeRawError_PrivateKeyRemoval(t *testing.T) {
	input := "error: -----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAK...\n-----END RSA PRIVATE KEY-----"
	got := sanitizeRawError(input)
	if strings.Contains(got, "BEGIN RSA PRIVATE KEY") {
		t.Errorf("PEM header not redacted: %s", got)
	}
	if strings.Contains(got, "MIIEpAIBAAK") {
		t.Errorf("PEM content not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED_PRIVATE_KEY]") {
		t.Errorf("expected [REDACTED_PRIVATE_KEY] placeholder: %s", got)
	}
}

func TestSanitizeRawError_PasswordParamRedaction(t *testing.T) {
	input := "failed with password=supersecret and api_key=abc123"
	got := sanitizeRawError(input)
	if strings.Contains(got, "supersecret") {
		t.Errorf("password value not redacted: %s", got)
	}
	if strings.Contains(got, "abc123") {
		t.Errorf("api_key value not redacted: %s", got)
	}
	if !strings.Contains(got, "password=[REDACTED]") {
		t.Errorf("expected password=[REDACTED] placeholder: %s", got)
	}
	if !strings.Contains(got, "api_key=[REDACTED]") {
		t.Errorf("expected api_key=[REDACTED] placeholder: %s", got)
	}
}

func TestSanitizeRawError_StackTraceRemoval(t *testing.T) {
	input := "panic: runtime error\ngoroutine 1 [running]:\n    /path/to/file.go:42"
	got := sanitizeRawError(input)
	if strings.Contains(got, "goroutine 1") {
		t.Errorf("goroutine header not redacted: %s", got)
	}
	if strings.Contains(got, "file.go:42") {
		t.Errorf("stack trace location not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED_STACK_TRACE]") {
		t.Errorf("expected [REDACTED_STACK_TRACE] placeholder: %s", got)
	}
}

func TestSanitizeRawError_PhoneRedaction(t *testing.T) {
	input := "contact: +86-138-1234-5678 or +1 (555) 123-4567"
	got := sanitizeRawError(input)
	if strings.Contains(got, "138-1234-5678") {
		t.Errorf("Chinese phone number not redacted: %s", got)
	}
	if strings.Contains(got, "555") {
		t.Errorf("US phone number not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED_PHONE]") {
		t.Errorf("expected [REDACTED_PHONE] placeholder: %s", got)
	}
}

// ---------------------------------------------------------------------------
// WithSkipErrorMessage / ShouldSkipErrorMessage tests
// ---------------------------------------------------------------------------

func TestWithSkipErrorMessage_ContextPropagation(t *testing.T) {
	ctx := context.Background()
	if turnagent.ShouldSkipErrorMessage(ctx) {
		t.Error("plain context should not have skip flag")
	}

	skipCtx := turnagent.WithSkipErrorMessage(ctx)
	if !turnagent.ShouldSkipErrorMessage(skipCtx) {
		t.Error("WithSkipErrorMessage context should have skip flag = true")
	}

	// Original context is not mutated.
	if turnagent.ShouldSkipErrorMessage(ctx) {
		t.Error("original context should not be mutated")
	}
}

func TestWithSkipErrorMessage_DefaultFalse(t *testing.T) {
	ctx := context.Background()
	if turnagent.ShouldSkipErrorMessage(ctx) {
		t.Error("default context should return false")
	}
}

func TestWithSkipErrorMessage_NestedContext(t *testing.T) {
	// Derived contexts should inherit the skip flag.
	type testKey struct{}
	ctx := context.Background()
	skipCtx := turnagent.WithSkipErrorMessage(ctx)
	derivedCtx := context.WithValue(skipCtx, testKey{}, "value")
	if !turnagent.ShouldSkipErrorMessage(derivedCtx) {
		t.Error("derived context should inherit skip flag")
	}
}
