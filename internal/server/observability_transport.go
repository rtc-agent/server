// observability_transport.go provides an http.RoundTripper that intercepts
// every LLM API request/response for metrics collection, and optionally
// logs full payloads to logs/llm-payload.log (JSON Lines format).
//
// The transport ALWAYS wraps the HTTP client regardless of payload logging,
// guaranteeing 100% coverage for metrics (request count, latency, token usage).
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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"go.uber.org/zap"

	pkglogger "github.com/rtc-agent/server/pkg/logger"
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
	//   - stream: whether this was a streaming (SSE) response
	Observe(req *http.Request, model string, statusCode int, elapsed time.Duration, usage TokenUsage, stream bool)
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

	// --- Handle request body ---
	// We need to buffer the request body so we can:
	// 1. Extract model name (for metrics)
	// 2. Log it (if payloadLog=true)
	// 3. Restore it for the actual transport to read
	var reqBodyBytes []byte
	if req.Body != nil {
		var err error
		reqBodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			t.logError("llm.http.request_read_error", req.URL.String(), err)
		}
		_ = req.Body.Close()
		// Restore body so the actual transport can read it
		req.Body = io.NopCloser(bytes.NewReader(reqBodyBytes))
	}

	// Extract model name from request body (for metrics)
	model := extractModelFromBody(reqBodyBytes)

	// --- Payload logging: request ---
	if t.payloadLog {
		l := t.payloadLogger()
		if l != nil {
			reqFields := []zap.Field{
				zap.String("event", "llm.http.request"),
				zap.String("method", req.Method),
				zap.String("url", req.URL.String()),
				zap.String("host", req.URL.Host),
				zap.Time("time", start),
			}
			reqFields = append(reqFields, zap.Any("request_headers", redactHeaders(req.Header)))
			if len(reqBodyBytes) > 0 {
				reqFields = append(reqFields, zap.String("request_body", compactJSON(reqBodyBytes)))
			}
			l.Info("llm.http.request", reqFields...)
		}
	}

	// --- Execute the actual request ---
	// Attach an httptrace to capture connection timing
	var gotConnTime time.Time
	traceCtx := httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(ci httptrace.GotConnInfo) {
			gotConnTime = time.Now()
			_ = ci // unused beyond timing
		},
	})
	req = req.WithContext(traceCtx)

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		t.logError("llm.http.error", req.URL.String(), err)
		return nil, err
	}

	_ = gotConnTime // available for future use
	elapsed := time.Since(start)

	// --- Handle response ---
	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(contentType, "text/event-stream")

	if isSSE {
		// For streaming responses, wrap the body to observe chunks as consumed.
		// Token usage is extracted from the accumulated stream data on EOF.
		resp.Body = &observabilityReadCloser{
			rc:           resp.Body,
			url:          req.URL.String(),
			model:        model,
			payloadLog:   t.payloadLog,
			observer:     t.observer,
			req:          req,
			startTime:    start,
			statusCode:   resp.StatusCode,
			isStream:     true,
		}
		// Payload logging for streaming
		if t.payloadLog {
			l := t.payloadLogger()
			if l != nil {
				respFields := []zap.Field{
					zap.String("event", "llm.http.response"),
					zap.String("url", req.URL.String()),
					zap.Int("status_code", resp.StatusCode),
					zap.Duration("elapsed", elapsed),
					zap.Any("response_headers", resp.Header),
					zap.Bool("stream", true),
				}
				l.Debug("llm.http.response (streaming)", respFields...)
			}
		}
	} else {
		// For non-streaming responses, read the full body.
		var respBodyBytes []byte
		if resp.Body != nil {
			respBodyBytes, err = io.ReadAll(resp.Body)
			if err != nil {
				t.logError("llm.http.response_body_read_error", req.URL.String(), err)
			}
			_ = resp.Body.Close()
			// Restore body so the caller can read it
			resp.Body = io.NopCloser(bytes.NewReader(respBodyBytes))
		}

		// Extract token usage from response
		usage := extractTokenUsage(respBodyBytes)

		// Notify observer
		if t.observer != nil {
			t.observer.Observe(req, model, resp.StatusCode, elapsed, usage, false)
		}

		// Payload logging: response
		if t.payloadLog {
			l := t.payloadLogger()
			if l != nil {
				respFields := []zap.Field{
					zap.String("event", "llm.http.response"),
					zap.String("url", req.URL.String()),
					zap.Int("status_code", resp.StatusCode),
					zap.Duration("elapsed", elapsed),
					zap.Any("response_headers", resp.Header),
				}
				if len(respBodyBytes) > 0 {
					respFields = append(respFields, zap.String("response_body", compactJSON(respBodyBytes)))
				}
				l.Debug("llm.http.response", respFields...)
			}
		}
	}

	return resp, nil
}

// payloadLogger returns the payload logger, or nil if not initialized.
func (t *observabilityTransport) payloadLogger() *zap.Logger {
	return pkglogger.LLMPayload()
}

// logError logs an error to the payload logger (if available) or drops it.
func (t *observabilityTransport) logError(event, url string, err error) {
	if t.payloadLog {
		l := t.payloadLogger()
		if l != nil {
			l.Error(event,
				zap.String("event", event),
				zap.String("url", url),
				zap.Error(err),
			)
		}
	}
}

// observabilityReadCloser wraps an io.ReadCloser for streaming LLM responses.
// It observes each chunk (optionally logging payloads) and extracts token
// usage from the accumulated stream on EOF.
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
	buf        bytes.Buffer // accumulates the full response for token usage extraction & final log
}

func (orc *observabilityReadCloser) Read(p []byte) (int, error) {
	n, err := orc.rc.Read(p)
	if n > 0 {
		// Accumulate for final token usage extraction and log
		orc.buf.Write(p[:n])

		// Payload logging: log each chunk
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
		if orc.payloadLog {
			l := pkglogger.LLMPayload()
			if l != nil {
				l.Error("llm.http.stream_read_error",
					zap.String("url", orc.url),
					zap.Error(err),
				)
			}
		}
	}
	if err == io.EOF {
		// Extract token usage from the accumulated stream
		usage := extractTokenUsage(orc.buf.Bytes())

		// Notify observer
		if orc.observer != nil {
			orc.observer.Observe(orc.req, orc.model, orc.statusCode, time.Since(orc.startTime), usage, orc.isStream)
		}

		// Payload logging: log the accumulated full response
		if orc.payloadLog && orc.buf.Len() > 0 {
			l := pkglogger.LLMPayload()
			if l != nil {
				l.Info("llm.http.response (stream complete)",
					zap.String("event", "llm.http.response"),
					zap.String("url", orc.url),
					zap.Bool("stream", true),
					zap.Bool("stream_complete", true),
					zap.String("response_body", compactJSON(orc.buf.Bytes())),
					zap.Int("total_bytes", orc.buf.Len()),
				)
			}
		}
	}
	return n, err
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
