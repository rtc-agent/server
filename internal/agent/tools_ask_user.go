package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// askUserTool implements Claude Code's AskUserQuestion tool semantics over RTC.
//
// The LLM supplies 1-4 multiple-choice questions; the client renders them in a
// selection UI and submits the user's answers via the RTC result channel. The
// server then assembles a human-readable "User has answered your questions: ..."
// string (matching Claude Code's mapToolResultToToolResultBlockParam) and
// returns it to the agent.
//
// Schema alignment with Claude Code (src/tools/AskUserQuestionTool/):
//   - questions: 1-4 items
//   - question.question: full question ending in '?'
//   - question.header: short chip label (≤12 chars)
//   - question.options: 2-4 items {label, description, preview?}
//   - question.multiSelect: bool, default false
//
// answers/annotations/metadata are NOT part of the LLM input: in Claude Code
// they are internal plumbing filled by the permission component before call().
// In RTC they travel the client→server result channel instead.
type askUserTool struct{ base *rtcToolBase }

// askUser input types — the schema is built manually via schema.ParameterInfo
// so Eino emits the right JSON Schema.
type (
	// askUserResult is the shape the client must submit as RTC.Result for a
	// completed ask_user call.
	askUserResult struct {
		Answers     map[string]string                 `json:"answers"`
		Annotations map[string]askUserAnnotation      `json:"annotations,omitempty"`
		Metadata    *askUserMetadata                  `json:"metadata,omitempty"`
	}
	askUserAnnotation struct {
		Preview string `json:"preview,omitempty"`
		Notes   string `json:"notes,omitempty"`
	}
	askUserMetadata struct {
		Source string `json:"source,omitempty"`
	}
)

func (t *askUserTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "ask_user",
		Desc: "Asks the user 1-4 multiple-choice questions to gather preferences, clarify ambiguity, understand requirements, or get decisions on implementation choices. Users can always pick 'Other' to provide free-form text.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"questions": {
				Type:     schema.Array,
				Desc:     "Questions to ask the user (1-4 questions). Each question has a question text, a short header chip label, 2-4 options, and an optional multiSelect flag.",
				Required: true,
				ElemInfo: &schema.ParameterInfo{
					Type: schema.Object,
					SubParams: map[string]*schema.ParameterInfo{
						"question": {
							Type:     schema.String,
							Desc:     "The complete question to ask. Should be clear, specific, and end with a question mark. Example: \"Which library should we use for date formatting?\" If multiSelect is true, phrase accordingly, e.g. \"Which features do you want to enable?\"",
							Required: true,
						},
						"header": {
							Type:     schema.String,
							Desc:     "Very short label (max 12 chars) displayed as a chip/tag next to the question. Examples: \"Auth method\", \"Library\", \"Approach\".",
							Required: true,
						},
						"options": {
							Type:     schema.Array,
							Desc:     "Available choices for this question. Must have 2-4 options. Each option should be a distinct choice. Do NOT include an 'Other' option — that is provided automatically by the UI.",
							Required: true,
							ElemInfo: &schema.ParameterInfo{
								Type: schema.Object,
								SubParams: map[string]*schema.ParameterInfo{
									"label": {
										Type:     schema.String,
										Desc:     "Display text for this option (1-5 words). Concise and clearly describes the choice.",
										Required: true,
									},
									"description": {
										Type:     schema.String,
										Desc:     "Explanation of what this option means or what will happen if chosen. Provides context about trade-offs or implications.",
										Required: true,
									},
									"preview": {
										Type:     schema.String,
										Desc:     "Optional preview content rendered when this option is focused. Use for ASCII mockups, code snippets, diagram variations, or configuration examples to help users visually compare options. Multi-line text supported. Only supported for single-select questions.",
										Required: false,
									},
								},
							},
						},
						"multiSelect": {
							Type:     schema.Boolean,
							Desc:     "Set to true to allow the user to select multiple options instead of just one. Use when choices are not mutually exclusive. Defaults to false.",
							Required: false,
						},
					},
				},
			},
		}),
	}, nil
}

func (t *askUserTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "ask_user", argumentsInJSON, opts...)
}

// formatAskUserResult converts the client-submitted RTC result into the text
// that will be fed back to the LLM. The format mirrors Claude Code's
// mapToolResultToToolResultBlockParam:
//
//	User has answered your questions: "Q1?"="A1", "Q2?"="A2". You can now continue with the user's answers in mind.
//
// Per-question annotations (preview / notes) are appended after the answer.
//
// On status != completed, falls back to the default formatToolCallOutput so
// errors/timeouts/rejections are reported consistently with other RTC tools.
func formatAskUserResult(dbRtc *model.Rtc) string {
	status := protocol.RtcStatus(dbRtc.Status)

	// Non-completed statuses: delegate to the generic formatter so errors,
	// timeouts, and rejections are reported identically to other RTC tools.
	if status != protocol.RtcStatusCompleted {
		output := string(dbRtc.Result)
		if status == protocol.RtcStatusFailed && dbRtc.ErrorMessage != "" {
			output = dbRtc.ErrorMessage
		}
		statusStr := string(status)
		return formatToolCallOutput(protocol.ToolCall{
			ToolName: dbRtc.ToolName,
			Output:   &output,
			Status:   &statusStr,
		})
	}

	if len(dbRtc.Result) == 0 {
		return "User has answered your questions: (no answers received). You can now continue."
	}

	var result askUserResult
	if err := json.Unmarshal([]byte(dbRtc.Result), &result); err != nil {
		// Client submitted non-JSON result: treat as opaque text.
		return fmt.Sprintf("User has answered your questions: %s. You can now continue with the user's answers in mind.", string(dbRtc.Result))
	}

	if len(result.Answers) == 0 {
		return "User has answered your questions: (no answers received). You can now continue."
	}

	// Build answer entries preserving Claude Code's format.
	parts := make([]string, 0, len(result.Answers))
	for q, a := range result.Answers {
		entry := fmt.Sprintf("%q=%q", q, a)
		if ann, ok := result.Annotations[q]; ok {
			if ann.Preview != "" {
				entry += fmt.Sprintf(" (selected preview:\n%s)", ann.Preview)
			}
			if ann.Notes != "" {
				entry += fmt.Sprintf(" (user notes: %s)", ann.Notes)
			}
		}
		parts = append(parts, entry)
	}

	return fmt.Sprintf("User has answered your questions: %s. You can now continue with the user's answers in mind.",
		strings.Join(parts, ", "))
}
