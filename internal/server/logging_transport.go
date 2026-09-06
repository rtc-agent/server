// logging_transport.go provides an http.RoundTripper that logs full LLM API
// request/response payloads to logs/llm-payload.log (JSON Lines format).
//
// This operates at the HTTP transport layer, capturing exactly what is sent to
// and received from the LLM provider (Anthropic/OpenAI), including:
//   - Full request body: system prompt, tools (in API-native format), messages,
//     all parameters (temperature, max_tokens, thinking config, etc.)
//   - Full response body: model output, token usage, finish reason
//   - For streaming responses (SSE): logs each chunk as it is consumed
//
// The transport wraps the default http.DefaultTransport so connection pooling,
// TLS, and other standard behavior are preserved.
//
// Wire-up: inject the logging HTTPClient into eino-ext's claude.Config /
// openai.ChatModelConfig in internal/server/llm.go when log.llm_payload=true.

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

// loggingTransport wraps an http.RoundTripper and logs the full request/response.
type loggingTransport struct {
	base http.RoundTripper
}

// newLoggingTransport creates a logging transport wrapping the given base.
// If base is nil, http.DefaultTransport is used.
func newLoggingTransport(base http.RoundTripper) *loggingTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &loggingTransport{base: base}
}

// newLoggingHTTPClient returns an *http.Client with the logging transport.
// Returns nil if the llm-payload logger is not initialized.
func newLoggingHTTPClient() *http.Client {
	if pkglogger.LLMPayload() == nil {
		return nil
	}
	return &http.Client{
		Transport: newLoggingTransport(nil),
	}
}

// RoundTrip implements http.RoundTripper.
func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	l := pkglogger.LLMPayload()
	start := time.Now()

	// --- Log request ---
	var reqBodyBytes []byte
	if req.Body != nil {
		var err error
		reqBodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			l.Error("llm.http.request_read_error", zap.Error(err))
		}
		_ = req.Body.Close()
		// Restore body so the actual transport can read it
		req.Body = io.NopCloser(bytes.NewReader(reqBodyBytes))
	}

	reqFields := []zap.Field{
		zap.String("event", "llm.http.request"),
		zap.String("method", req.Method),
		zap.String("url", req.URL.String()),
		zap.String("host", req.URL.Host),
		zap.Time("time", start),
	}
	// Add request headers (redact Authorization for safety)
	reqFields = append(reqFields, zap.Any("request_headers", redactHeaders(req.Header)))
	if len(reqBodyBytes) > 0 {
		reqFields = append(reqFields, zap.String("request_body", compactJSON(reqBodyBytes)))
	}
	l.Info("llm.http.request", reqFields...)

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
		l.Error("llm.http.error",
			zap.String("event", "llm.http.error"),
			zap.String("url", req.URL.String()),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err),
		)
		return nil, err
	}

	_ = gotConnTime // available for future use

	// --- Log response ---
	respFields := []zap.Field{
		zap.String("event", "llm.http.response"),
		zap.String("url", req.URL.String()),
		zap.Int("status_code", resp.StatusCode),
		zap.Duration("elapsed", time.Since(start)),
		zap.Any("response_headers", resp.Header),
	}

	// For streaming responses (SSE), wrap the body to log chunks as consumed.
	// For non-streaming, read the full body and log it.
	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(contentType, "text/event-stream")

	if isSSE {
		// Wrap the body: each Read() call is tee'd through a logger
		resp.Body = &loggingReadCloser{
			rc:     resp.Body,
			logger: l,
			url:    req.URL.String(),
		}
		respFields = append(respFields, zap.Bool("stream", true))
		l.Debug("llm.http.response (streaming)", respFields...)
	} else {
		var respBodyBytes []byte
		if resp.Body != nil {
			respBodyBytes, err = io.ReadAll(resp.Body)
			if err != nil {
				respFields = append(respFields, zap.String("body_read_error", err.Error()))
			}
			_ = resp.Body.Close()
			// Restore body so the caller can read it
			resp.Body = io.NopCloser(bytes.NewReader(respBodyBytes))
		}
		if len(respBodyBytes) > 0 {
			respFields = append(respFields, zap.String("response_body", compactJSON(respBodyBytes)))
		}
		l.Debug("llm.http.response", respFields...)
	}

	return resp, nil
}

// loggingReadCloser wraps an io.ReadCloser and logs each chunk as it is read.
// Used for SSE (streaming) LLM responses.
type loggingReadCloser struct {
	rc     io.ReadCloser
	logger *zap.Logger
	url    string
	buf    bytes.Buffer // accumulates the full response for final summary
}

func (lrc *loggingReadCloser) Read(p []byte) (int, error) {
	n, err := lrc.rc.Read(p)
	if n > 0 {
		// Accumulate for final log
		lrc.buf.Write(p[:n])
		// Log each chunk
		lrc.logger.Debug("llm.http.stream_chunk",
			zap.String("event", "llm.http.stream_chunk"),
			zap.String("url", lrc.url),
			zap.String("chunk", string(p[:n])),
			zap.Int("bytes", n),
		)
	}
	if err != nil && err != io.EOF {
		lrc.logger.Error("llm.http.stream_read_error",
			zap.String("url", lrc.url),
			zap.Error(err),
		)
	}
	if err == io.EOF {
		// Log the accumulated full response
		if lrc.buf.Len() > 0 {
			lrc.logger.Info("llm.http.response (stream complete)",
				zap.String("event", "llm.http.response"),
				zap.String("url", lrc.url),
				zap.Bool("stream", true),
				zap.Bool("stream_complete", true),
				zap.String("response_body", compactJSON(lrc.buf.Bytes())),
				zap.Int("total_bytes", lrc.buf.Len()),
			)
		}
	}
	return n, err
}

func (lrc *loggingReadCloser) Close() error {
	return lrc.rc.Close()
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
