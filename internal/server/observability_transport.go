// observability_transport.go provides an http.RoundTripper that intercepts
// every LLM API request/response for metrics collection, and optionally
// logs full payloads to logs/llm-payload.log (JSON Lines format).
//
// The transport ALWAYS wraps the HTTP client regardless of payload logging,
// guaranteeing 100% coverage for metrics (request count, latency, token usage).
//
// SECURITY WARNING: Payload Logging (payloadLog=true)
//
// When payloadLog is enabled, the FULL request and response bodies are logged,
// including:
//   - System prompts (may contain proprietary instructions)
//   - User messages (may contain sensitive user data, PII, secrets)
//   - Model outputs (may contain generated sensitive content)
//   - All parameters and metadata
//
// Production Deployment Guidelines:
//   - DO NOT enable payloadLog in production unless required for debugging
//   - If enabled, ensure logs are stored securely with restricted access
//   - Rotate logs frequently and apply retention policies
//   - Consider redacting specific fields if only partial logging is needed
//   - Monitor log volume - full payload logging can generate significant storage
//
// For routine observability, use metrics and structured logging instead.
//
// When payload logging is enabled (payloadLog=true), it captures:
//   - Full request body: system prompt, tools, messages, all parameters
//   - Full response body: model output, token usage, finish reason
//   - For streaming responses (SSE): logs each chunk as it is consumed
//
// The transport wraps the default http.DefaultTransport so connection pooling,
// TLS, and other standard behavior are preserved.
//
// Wire-up: inject the observability HTTPClient into eino-ext's claude.Config /
// openai.ChatModelConfig in internal/server/llm.go (always, not conditionally).

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	pkglogger "github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// TokenUsage represents LLM API token usage extracted from a response.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// IsEmpty reports whether the token usage contains no data.
func (u TokenUsage) IsEmpty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0
}

// LLMResponseObserver receives notifications about LLM API responses.
// Implementations can record metrics, traces, or other observability data.
//
// All methods must be safe for concurrent use.
type LLMResponseObserver interface {
	// Observe is called after each LLM API response completes.
	// For streaming responses, this is called once when the stream finishes.
	//
	// Parameters:
	//   - req: the original HTTP request
	//   - model: the model name extracted from the request body
	//   - statusCode: HTTP response status code
	//   - elapsed: wall-clock time from request start to response complete
	//   - usage: token usage extracted from the response (may be empty if
	//     the provider doesn't include usage or if parsing failed)
	Observe(req *http.Request, model string, statusCode int, elapsed time.Duration, usage TokenUsage)
}

// observabilityTransport wraps an http.RoundTripper and provides:
//  1. Token usage extraction from every response (for metrics)
//  2. Optional full payload logging (when payloadLog=true)
//  3. Observer notifications for external metrics collection
type observabilityTransport struct {
	base       http.RoundTripper
	payloadLog bool
	observer   LLMResponseObserver
}

// newObservabilityTransport creates an observability transport wrapping the given base.
// If base is nil, http.DefaultTransport is used.
func newObservabilityTransport(base http.RoundTripper, payloadLog bool, observer LLMResponseObserver) *observabilityTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	// Log a warning when payload logging is enabled to alert operators about
	// potential sensitive data exposure (P1 observability issue #9)
	if payloadLog {
		pkglogger.LLMPayload().Warn("LLM payload logging ENABLED - full request/response bodies will be logged. " +
			"Ensure logs are stored securely and access is restricted. " +
			"Consider disabling in production unless debugging.")
	}
	return &observabilityTransport{
		base:       base,
		payloadLog: payloadLog,
		observer:   observer,
	}
}

// newObservabilityHTTPClient returns an *http.Client with the observability transport.
// The transport is always created to ensure 100% metrics coverage.
// payloadLog controls whether full request/response payloads are logged.
// observer is optional; pass nil if no metrics collection is needed.
func newObservabilityHTTPClient(payloadLog bool, observer LLMResponseObserver) *http.Client {
	return &http.Client{
		Transport: newObservabilityTransport(nil, payloadLog, observer),
	}
}

// RoundTrip implements http.RoundTripper.
func (t *observabilityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	// Buffer request body so we can extract the model, log it, and restore it.
	reqBodyBytes := t.bufferRequestBody(req)
	model := extractModelFromBody(reqBodyBytes)

	// Payload logging: request.
	t.logRequestPayload(req, start, reqBodyBytes)

	// Execute the actual request.
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		t.logError("llm.http.error", req.URL.String(), err)
		return nil, err
	}

	elapsed := time.Since(start)

	// Dispatch to the appropriate response handler based on content type.
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		t.handleStreamingResponse(resp, req, model, start, elapsed)
	} else {
		t.handleBufferedResponse(resp, req, model, elapsed)
	}

	return resp, nil
}

// bufferRequestBody reads and restores the request body so it can be inspected
// (for model extraction / payload logging) and then re-read by the underlying transport.
func (t *observabilityTransport) bufferRequestBody(req *http.Request) []byte {
	if req.Body == nil {
		return nil
	}
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.logError("llm.http.request_read_error", req.URL.String(), err)
	}
	// req.Body.Close errors are intentionally ignored.
	// The body has been fully read, and close errors on request bodies are not actionable.
	_ = req.Body.Close()
	// Restore body so the actual transport can read it.
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return bodyBytes
}

// logRequestPayload logs the full request body/headers when payloadLog is enabled.
func (t *observabilityTransport) logRequestPayload(req *http.Request, start time.Time, bodyBytes []byte) {
	if !t.payloadLog {
		return
	}
	l := t.payloadLogger()
	if l == nil {
		return
	}
	fields := []zap.Field{
		zap.String("event", "llm.http.request"),
		zap.String("method", req.Method),
		zap.String("url", req.URL.String()),
		zap.String("host", req.URL.Host),
		zap.Time("time", start),
	}
	if sessionID := turnagent.SessionIDFromContext(req.Context()); sessionID != "" {
		fields = append(fields, zap.String("session_id", sessionID))
	}
	fields = append(fields, zap.Any("request_headers", redactHeaders(req.Header)))
	if len(bodyBytes) > 0 {
		fields = append(fields, zap.String("request_body", compactJSON(bodyBytes)))
	}
	l.Info("llm.http.request", fields...)
}

// handleStreamingResponse wraps the response body so chunks are observed as consumed.
// Token usage is extracted incrementally on EOF (no unbounded buffering).
func (t *observabilityTransport) handleStreamingResponse(resp *http.Response, req *http.Request, model string, start time.Time, elapsed time.Duration) {
	resp.Body = &observabilityReadCloser{
		rc:         resp.Body,
		url:        req.URL.String(),
		model:      model,
		payloadLog: t.payloadLog,
		observer:   t.observer,
		req:        req,
		startTime:  start,
		statusCode: resp.StatusCode,
		isStream:   true,
	}
	if !t.payloadLog {
		return
	}
	l := t.payloadLogger()
	if l == nil {
		return
	}
	fields := []zap.Field{
		zap.String("event", "llm.http.response"),
		zap.String("url", req.URL.String()),
		zap.Int("status_code", resp.StatusCode),
		zap.Duration("elapsed", elapsed),
		zap.Any("response_headers", resp.Header),
		zap.Bool("stream", true),
	}
	if sessionID := turnagent.SessionIDFromContext(req.Context()); sessionID != "" {
		fields = append(fields, zap.String("session_id", sessionID))
	}
	l.Debug("llm.http.response (streaming)", fields...)
}

// handleBufferedResponse reads a non-streaming response body into memory, extracts
// token usage, notifies the observer, and optionally logs the full payload.
func (t *observabilityTransport) handleBufferedResponse(resp *http.Response, req *http.Request, model string, elapsed time.Duration) {
	var respBodyBytes []byte
	if resp.Body != nil {
		var err error
		respBodyBytes, err = io.ReadAll(resp.Body)
		if err != nil {
			t.logError("llm.http.response_body_read_error", req.URL.String(), err)
		}
		_ = resp.Body.Close()
		// Restore body so the caller can read it.
		resp.Body = io.NopCloser(bytes.NewReader(respBodyBytes))
	}

	usage := extractTokenUsage(respBodyBytes)
	if t.observer != nil {
		t.observer.Observe(req, model, resp.StatusCode, elapsed, usage)
	}
	t.logResponsePayload(req, resp, elapsed, respBodyBytes)
}

// logResponsePayload logs the full response body/headers when payloadLog is enabled.
func (t *observabilityTransport) logResponsePayload(req *http.Request, resp *http.Response, elapsed time.Duration, bodyBytes []byte) {
	if !t.payloadLog {
		return
	}
	l := t.payloadLogger()
	if l == nil {
		return
	}
	fields := []zap.Field{
		zap.String("event", "llm.http.response"),
		zap.String("url", req.URL.String()),
		zap.Int("status_code", resp.StatusCode),
		zap.Duration("elapsed", elapsed),
		zap.Any("response_headers", resp.Header),
	}
	if sessionID := turnagent.SessionIDFromContext(req.Context()); sessionID != "" {
		fields = append(fields, zap.String("session_id", sessionID))
	}
	if len(bodyBytes) > 0 {
		fields = append(fields, zap.String("response_body", compactJSON(bodyBytes)))
	}
	l.Debug("llm.http.response", fields...)
}

// payloadLogger returns the payload logger, or nil if not initialized.
func (t *observabilityTransport) payloadLogger() *zap.Logger {
	return pkglogger.LLMPayload()
}

// logError logs errors using structured logging, independent of payloadLog setting.
// Errors are always logged at Warn level to ensure visibility in production environments.
func (t *observabilityTransport) logError(event, url string, err error) {
	// Errors are always logged regardless of payloadLog setting
	pkglogger.Warn(context.Background(), "LLM HTTP error",
		zap.String("event", event),
		zap.String("url", url),
		zap.Error(err),
	)
}

// observabilityReadCloser wraps an io.ReadCloser for streaming LLM responses.
// It performs incremental token usage extraction without accumulating the
// entire stream in memory, preventing memory exhaustion on long streams.
type observabilityReadCloser struct {
	rc         io.ReadCloser
	url        string
	model      string
	payloadLog bool
	observer   LLMResponseObserver
	req        *http.Request
	startTime  time.Time
	statusCode int
	isStream   bool

	// Incremental token extraction state (no unbounded buffer)
	inputTokens  int    // extracted from message_start (Anthropic) or usage chunk (OpenAI)
	outputTokens int    // extracted from message_delta (Anthropic) or usage chunk (OpenAI)
	lineBuf      []byte // accumulates partial lines for SSE parsing
	totalBytes   int64  // total bytes read (for logging)
}

func (orc *observabilityReadCloser) Read(p []byte) (int, error) {
	n, err := orc.rc.Read(p)
	if n > 0 {
		orc.totalBytes += int64(n)

		// Incrementally parse SSE events to extract token usage
		orc.extractTokensIncremental(p[:n])

		// Payload logging: log each chunk (when enabled)
		if orc.payloadLog {
			l := pkglogger.LLMPayload()
			if l != nil {
				l.Debug("llm.http.stream_chunk",
					zap.String("event", "llm.http.stream_chunk"),
					zap.String("url", orc.url),
					zap.String("chunk", string(p[:n])),
					zap.Int("bytes", n),
				)
			}
		}
	}
	if err != nil && err != io.EOF {
		// Stream read errors are always logged, independent of payloadLog
		pkglogger.Warn(context.Background(), "LLM HTTP stream read error",
			zap.String("url", orc.url),
			zap.Error(err),
		)
	}
	if errors.Is(err, io.EOF) {
		// Process any remaining line buffer
		orc.processLine(orc.lineBuf)
		orc.lineBuf = nil

		// Build usage from incrementally extracted tokens
		usage := TokenUsage{
			InputTokens:  orc.inputTokens,
			OutputTokens: orc.outputTokens,
		}

		// Notify observer
		if orc.observer != nil {
			orc.observer.Observe(orc.req, orc.model, orc.statusCode, time.Since(orc.startTime), usage)
		}

		// Payload logging: log stream completion summary (not full body)
		if orc.payloadLog {
			l := pkglogger.LLMPayload()
			if l != nil {
				fields := []zap.Field{
					zap.String("event", "llm.http.response"),
					zap.String("url", orc.url),
					zap.Bool("stream", true),
					zap.Bool("stream_complete", true),
					zap.Int64("total_bytes", orc.totalBytes),
					zap.Int("input_tokens", orc.inputTokens),
					zap.Int("output_tokens", orc.outputTokens),
				}
				// Add session ID from context if available
				if sessionID := turnagent.SessionIDFromContext(orc.req.Context()); sessionID != "" {
					fields = append(fields, zap.String("session_id", sessionID))
				}
				l.Info("llm.http.response (stream complete)", fields...)
			}
		}
	}
	return n, err
}

// extractTokensIncremental parses SSE data incrementally, extracting token
// usage without accumulating the entire stream. It handles both Anthropic
// and OpenAI streaming formats.
func (orc *observabilityReadCloser) extractTokensIncremental(data []byte) {
	// Append to any leftover partial line
	if len(orc.lineBuf) > 0 {
		data = append(orc.lineBuf, data...)
		orc.lineBuf = nil
	}

	// Process complete lines
	for len(data) > 0 {
		// Find newline
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			// Incomplete line, save for next read
			orc.lineBuf = append(orc.lineBuf, data...)
			return
		}

		line := data[:idx]
		data = data[idx+1:]

		orc.processLine(line)
	}
}

// processLine processes a single SSE line and extracts token usage.
func (orc *observabilityReadCloser) processLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}

	// Extract JSON data after "data:"
	jsonData := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if len(jsonData) == 0 {
		return
	}

	// Try Anthropic format: message_start contains input_tokens, message_delta contains output_tokens
	var anthropicEvent struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(jsonData, &anthropicEvent); err == nil {
		if anthropicEvent.Type == "message_start" && anthropicEvent.Message.Usage.InputTokens > 0 {
			orc.inputTokens = anthropicEvent.Message.Usage.InputTokens
		}
		if anthropicEvent.Type == "message_delta" && anthropicEvent.Usage.OutputTokens > 0 {
			orc.outputTokens = anthropicEvent.Usage.OutputTokens
		}
		// If we got Anthropic data, don't try OpenAI parsing
		if anthropicEvent.Type != "" {
			return
		}
	}

	// Try OpenAI format: usage in final chunk
	var openaiChunk struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(jsonData, &openaiChunk); err == nil {
		if openaiChunk.Usage.PromptTokens > 0 {
			orc.inputTokens = openaiChunk.Usage.PromptTokens
		}
		if openaiChunk.Usage.CompletionTokens > 0 {
			orc.outputTokens = openaiChunk.Usage.CompletionTokens
		}
	}
}

func (orc *observabilityReadCloser) Close() error {
	return orc.rc.Close()
}

// extractModelFromBody parses the model name from an LLM API request body.
// Supports both Anthropic and OpenAI formats.
// Returns "unknown" if parsing fails or no model is found.
func extractModelFromBody(body []byte) string {
	if len(body) == 0 {
		return "unknown"
	}

	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "unknown"
	}
	if payload.Model == "" {
		return "unknown"
	}
	return payload.Model
}

// extractTokenUsage parses token usage from an LLM API response body.
// Supports both Anthropic and OpenAI response formats, including streaming (SSE).
// Returns an empty TokenUsage if parsing fails or no usage is found.
func extractTokenUsage(body []byte) TokenUsage {
	if len(body) == 0 {
		return TokenUsage{}
	}

	// Try parsing as SSE stream first (for streaming responses)
	if usage := extractTokenUsageFromSSE(body); !usage.IsEmpty() {
		return usage
	}

	// Fall back to parsing as single JSON (for non-streaming responses)
	return extractTokenUsageFromJSON(body)
}

// extractTokenUsageFromSSE parses token usage from SSE stream data.
// For Anthropic: extracts from message_start (input_tokens) and message_delta (output_tokens)
// For OpenAI: extracts from the final chunk with usage field
func extractTokenUsageFromSSE(body []byte) TokenUsage {
	var inputTokens, outputTokens int

	// Parse SSE events line by line
	lines := bytes.Split(body, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}

		// Extract JSON data after "data:"
		jsonData := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(jsonData) == 0 {
			continue
		}

		// Try to parse as Anthropic format
		var anthropicEvent struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}

		if err := json.Unmarshal(jsonData, &anthropicEvent); err == nil {
			// message_start event: usage is in message.usage
			if anthropicEvent.Type == "message_start" && anthropicEvent.Message.Usage.InputTokens > 0 {
				inputTokens = anthropicEvent.Message.Usage.InputTokens
			}
			// message_delta event: usage is at top level
			if anthropicEvent.Type == "message_delta" && anthropicEvent.Usage.OutputTokens > 0 {
				outputTokens = anthropicEvent.Usage.OutputTokens
			}
		}

		// Try to parse as OpenAI format (usage in final chunk)
		var openaiChunk struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}

		if err := json.Unmarshal(jsonData, &openaiChunk); err == nil {
			if openaiChunk.Usage.PromptTokens > 0 {
				inputTokens = openaiChunk.Usage.PromptTokens
			}
			if openaiChunk.Usage.CompletionTokens > 0 {
				outputTokens = openaiChunk.Usage.CompletionTokens
			}
		}
	}

	return TokenUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}
}

// extractTokenUsageFromJSON parses token usage from a single JSON response.
// Supports both Anthropic and OpenAI formats.
func extractTokenUsageFromJSON(body []byte) TokenUsage {
	if len(body) == 0 {
		return TokenUsage{}
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return TokenUsage{}
	}

	usageRaw, ok := raw["usage"]
	if !ok {
		return TokenUsage{}
	}

	var usage struct {
		// Anthropic format
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		// OpenAI format
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
	if err := json.Unmarshal(usageRaw, &usage); err != nil {
		return TokenUsage{}
	}

	// Prefer Anthropic field names, fall back to OpenAI
	input := usage.InputTokens
	if input == 0 {
		input = usage.PromptTokens
	}
	output := usage.OutputTokens
	if output == 0 {
		output = usage.CompletionTokens
	}

	return TokenUsage{
		InputTokens:  input,
		OutputTokens: output,
	}
}

// redactHeaders returns a copy of headers with Authorization redacted.
func redactHeaders(h http.Header) map[string][]string {
	result := make(map[string][]string, len(h))
	for k, v := range h {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
			result[k] = []string{"***REDACTED***"}
		} else {
			result[k] = v
		}
	}
	return result
}

// compactJSON returns the JSON bytes as a compact string.
// If the input is not valid JSON, returns the raw string.
func compactJSON(b []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return string(b)
	}
	return buf.String()
}
