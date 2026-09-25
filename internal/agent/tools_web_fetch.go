package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/webfetch"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// webFetchTool — fetches web page content via pkg/webfetch.WebFetchManager
// ---------------------------------------------------------------------------

// webFetchTool implements Eino's InvokableTool interface for web fetch.
// The core fetch logic (SSRF protection, caching, HTML→Markdown conversion)
// lives in pkg/webfetch; this struct handles the InvokableTool contract,
// argument parsing, tracing, and message persistence.
type webFetchTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// webFetchArgs defines the JSON arguments accepted from the LLM.
type webFetchArgs struct {
	URL    string `json:"url"`
	Prompt string `json:"prompt"`
}

// createWebFetchTool returns a webFetchTool if WebFetchManager is configured.
func (h *helpers) createWebFetchTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	if h.deps.WebFetchManager == nil {
		return nil
	}
	return &webFetchTool{session: session, helpers: h, turnID: turnID}
}

// Info returns tool metadata used by the LLM to decide whether to call.
func (t *webFetchTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "webFetch",
		Desc: webFetchDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"url": {
				Type:     schema.String,
				Desc:     "The URL to fetch content from. Must be a fully-formed valid HTTP or HTTPS URL.",
				Required: true,
			},
			"prompt": {
				Type:     schema.String,
				Desc:     "The prompt to run on the fetched content. Describe what information you want to extract from the page.",
				Required: true,
			},
		}),
	}, nil
}

// InvokableRun executes the web fetch with JSON arguments from the LLM.
//
// Flow:
//  1. parseToolArgsWithPersist — parse JSON args (persist on parse failure)
//  2. Validate URL and prompt
//  3. Call WebFetchManager.Fetch()
//  4. publishToolMessages — persist input+output (on both success and failure)
//  5. Return formatted text to the LLM (via FormatFetchResponse)
func (t *webFetchTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.webFetch",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	// 1. Parse arguments.
	var args webFetchArgs
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "webFetch", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	// 2. Validate URL.
	args.URL = strings.TrimSpace(args.URL)
	if args.URL == "" {
		span.SetStatus(codes.Error, "url_required")
		errMsg := "Error: url is required and cannot be empty"
		if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          t.turnID,
			ToolName:        "webFetch",
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      errMsg,
		}); publishErr != nil {
			t.helpers.logger.Warn(ctx, "webFetch.persist_validation_error_failed", map[string]any{
				"error": publishErr.Error(),
			})
		}
		return errMsg, nil
	}

	span.SetAttributes(
		attribute.String("url", args.URL),
		attribute.Int("prompt_length", len(args.Prompt)),
	)

	// 3. Execute fetch.
	req := &webfetch.FetchRequest{
		URL:       args.URL,
		Prompt:    args.Prompt,
		SessionID: t.session.ID.String(),
	}

	resp, err := t.helpers.deps.WebFetchManager.Fetch(ctx, req)
	if err != nil {
		errMsg := publishToolError(
			ctx, span, t.helpers, t.session, t.turnID,
			"webFetch",
			"webFetch.fetch_failed",
			"webFetch.persist_fetch_error_failed",
			map[string]any{"url": args.URL, "error": err.Error()},
			err,
			argumentsInJSON,
		)
		return errMsg, nil
	}

	// 4. Format the response for the LLM.
	resultText := webfetch.FormatFetchResponse(resp)

	// Persist input + output messages.
	// Note: webFetch persists formatted text (not JSON) as ResultJSON because
	// the FetchResponse.Result may contain large text content; wrapping it in
	// JSON adds unnecessary token overhead for the LLM.
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "webFetch",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultText,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		return "", fmt.Errorf("webFetch: publish messages: %w", err)
	}

	span.SetAttributes(
		attribute.Int("bytes", int(resp.Bytes)),
		attribute.Int("status_code", resp.Code),
	)

	t.helpers.logger.Info(ctx, "webFetch.completed", map[string]any{
		"session_id":  t.session.ID.String(),
		"url":         args.URL,
		"bytes":       resp.Bytes,
		"status_code": resp.Code,
		"cached":      resp.Cached,
	})

	// 5. Return formatted text to the LLM.
	return resultText, nil
}
