package server

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// mockReadCloser wraps a bytes.Reader for testing.
type mockReadCloser struct {
	*bytes.Reader
}

func (m *mockReadCloser) Close() error { return nil }

func newTestORC(stream string) *observabilityReadCloser {
	return &observabilityReadCloser{
		rc:         &mockReadCloser{bytes.NewReader([]byte(stream))},
		url:        "http://test.com",
		model:      "claude-3",
		startTime:  time.Now(),
		statusCode: 200,
		isStream:   true,
	}
}

// readAll reads the entire stream until EOF.
func readAll(orc *observabilityReadCloser, bufSize int) {
	buf := make([]byte, bufSize)
	for {
		_, err := orc.Read(buf)
		if err == io.EOF {
			return
		}
		if err != nil {
			panic(err)
		}
	}
}

func TestObservabilityReadCloser_AnthropicStream(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":100}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"text":"Hello"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":50}}

event: message_stop
data: {"type":"message_stop"}

`
	orc := newTestORC(stream)
	readAll(orc, 4096)

	if orc.inputTokens != 100 {
		t.Errorf("inputTokens = %d, want 100", orc.inputTokens)
	}
	if orc.outputTokens != 50 {
		t.Errorf("outputTokens = %d, want 50", orc.outputTokens)
	}
}

func TestObservabilityReadCloser_OpenAIStream(t *testing.T) {
	stream := `data: {"id":"chatcmpl-123","choices":[{"delta":{"content":"Hello"}}]}

data: {"id":"chatcmpl-123","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":200,"completion_tokens":75}}

data: [DONE]

`
	orc := newTestORC(stream)
	readAll(orc, 4096)

	if orc.inputTokens != 200 {
		t.Errorf("inputTokens = %d, want 200", orc.inputTokens)
	}
	if orc.outputTokens != 75 {
		t.Errorf("outputTokens = %d, want 75", orc.outputTokens)
	}
}

func TestObservabilityReadCloser_ChunkedReads(t *testing.T) {
	stream := `data: {"type":"message_start","message":{"usage":{"input_tokens":150}}}

data: {"type":"message_delta","usage":{"output_tokens":25}}

`
	orc := newTestORC(stream)
	// Small buffer forces multiple reads and tests partial line handling
	readAll(orc, 32)

	if orc.inputTokens != 150 {
		t.Errorf("inputTokens = %d, want 150", orc.inputTokens)
	}
	if orc.outputTokens != 25 {
		t.Errorf("outputTokens = %d, want 25", orc.outputTokens)
	}
}

func TestObservabilityReadCloser_NoUsage(t *testing.T) {
	stream := `data: {"type":"content_block_delta","delta":{"text":"Hello"}}

`
	orc := newTestORC(stream)
	readAll(orc, 4096)

	if orc.inputTokens != 0 {
		t.Errorf("inputTokens = %d, want 0", orc.inputTokens)
	}
	if orc.outputTokens != 0 {
		t.Errorf("outputTokens = %d, want 0", orc.outputTokens)
	}
}

func TestObservabilityReadCloser_TotalBytesTracked(t *testing.T) {
	stream := `data: {"type":"message_start","message":{"usage":{"input_tokens":100}}}`
	orc := newTestORC(stream)
	readAll(orc, 4096)

	if orc.totalBytes != int64(len(stream)) {
		t.Errorf("totalBytes = %d, want %d", orc.totalBytes, len(stream))
	}
}

func TestObservabilityReadCloser_MalformedJSON(t *testing.T) {
	stream := `data: {invalid json}
data: {"type":"message_start","message":{"usage":{"input_tokens":50}}}
`
	orc := newTestORC(stream)
	// Should not panic
	readAll(orc, 4096)

	if orc.inputTokens != 50 {
		t.Errorf("inputTokens = %d, want 50", orc.inputTokens)
	}
}

func TestProcessLine(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantInput  int
		wantOutput int
	}{
		{
			name:       "non-data line",
			line:       "event: message_start",
			wantInput:  0,
			wantOutput: 0,
		},
		{
			name:       "empty data",
			line:       "data:",
			wantInput:  0,
			wantOutput: 0,
		},
		{
			name:       "whitespace data",
			line:       "data:   ",
			wantInput:  0,
			wantOutput: 0,
		},
		{
			name:       "anthropic message_start",
			line:       `data: {"type":"message_start","message":{"usage":{"input_tokens":42}}}`,
			wantInput:  42,
			wantOutput: 0,
		},
		{
			name:       "anthropic message_delta",
			line:       `data: {"type":"message_delta","usage":{"output_tokens":17}}`,
			wantInput:  0,
			wantOutput: 17,
		},
		{
			name:       "openai usage",
			line:       `data: {"usage":{"prompt_tokens":100,"completion_tokens":50}}`,
			wantInput:  100,
			wantOutput: 50,
		},
		{
			name:       "invalid json",
			line:       `data: {not json}`,
			wantInput:  0,
			wantOutput: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orc := &observabilityReadCloser{}
			orc.processLine([]byte(tt.line))
			if orc.inputTokens != tt.wantInput {
				t.Errorf("inputTokens = %d, want %d", orc.inputTokens, tt.wantInput)
			}
			if orc.outputTokens != tt.wantOutput {
				t.Errorf("outputTokens = %d, want %d", orc.outputTokens, tt.wantOutput)
			}
		})
	}
}
