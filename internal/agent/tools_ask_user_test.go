package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// newTestRtc builds a minimal *model.Rtc with the given status/result for testing formatAskUserResult.
func newTestRtc(status protocol.RtcStatus, resultJSON string, errMsg string) *model.Rtc {
	return &model.Rtc{
		ToolName:     "ask_user",
		Status:       string(status),
		Result:       model.JSONBString(resultJSON),
		ErrorMessage: errMsg,
	}
}

func TestFormatAskUserResult_Completed_SingleAnswer(t *testing.T) {
	payload := map[string]any{
		"answers": map[string]string{"Which auth?": "OAuth 2.0"},
	}
	b, _ := json.Marshal(payload)

	got := formatAskUserResult(newTestRtc(protocol.RtcStatusCompleted, string(b), ""))

	want := `User has answered your questions: "Which auth?"="OAuth 2.0". You can now continue with the user's answers in mind.`
	if got != want {
		t.Errorf("format mismatch\n got:  %s\n want: %s", got, want)
	}
}

func TestFormatAskUserResult_Completed_MultiAnswer(t *testing.T) {
	payload := map[string]any{
		"answers": map[string]string{
			"Which auth?":    "OAuth 2.0",
			"Which layout?": "Stack, Cluster",
		},
	}
	b, _ := json.Marshal(payload)

	got := formatAskUserResult(newTestRtc(protocol.RtcStatusCompleted, string(b), ""))

	if !strings.HasPrefix(got, "User has answered your questions: ") {
		t.Errorf("missing header: %s", got)
	}
	if !strings.Contains(got, `"Which auth?"="OAuth 2.0"`) {
		t.Errorf("missing auth answer: %s", got)
	}
	if !strings.Contains(got, `"Which layout?"="Stack, Cluster"`) {
		t.Errorf("missing layout answer: %s", got)
	}
	if !strings.HasSuffix(got, ". You can now continue with the user's answers in mind.") {
		t.Errorf("missing footer: %s", got)
	}
}

func TestFormatAskUserResult_Completed_WithAnnotations(t *testing.T) {
	payload := map[string]any{
		"answers": map[string]string{"Auth?": "OAuth"},
		"annotations": map[string]any{
			"Auth?": map[string]any{
				"preview": "const x = 1;",
				"notes":   "user liked this",
			},
		},
	}
	b, _ := json.Marshal(payload)

	got := formatAskUserResult(newTestRtc(protocol.RtcStatusCompleted, string(b), ""))

	if !strings.Contains(got, `"Auth?"="OAuth"`) {
		t.Errorf("missing answer: %s", got)
	}
	if !strings.Contains(got, "selected preview:") {
		t.Errorf("missing preview annotation: %s", got)
	}
	if !strings.Contains(got, "const x = 1;") {
		t.Errorf("preview content missing: %s", got)
	}
	if !strings.Contains(got, "user notes: user liked this") {
		t.Errorf("missing user notes: %s", got)
	}
}

func TestFormatAskUserResult_Completed_EmptyResult(t *testing.T) {
	got := formatAskUserResult(newTestRtc(protocol.RtcStatusCompleted, "", ""))
	if !strings.Contains(got, "no answers received") {
		t.Errorf("expected 'no answers received' for empty result, got: %s", got)
	}
}

func TestFormatAskUserResult_Completed_EmptyAnswersDict(t *testing.T) {
	payload := map[string]any{"answers": map[string]string{}}
	b, _ := json.Marshal(payload)
	got := formatAskUserResult(newTestRtc(protocol.RtcStatusCompleted, string(b), ""))
	if !strings.Contains(got, "no answers received") {
		t.Errorf("expected 'no answers received' for empty dict, got: %s", got)
	}
}

func TestFormatAskUserResult_Completed_NonJSONFallback(t *testing.T) {
	// Client submitted opaque text instead of JSON — should not crash.
	got := formatAskUserResult(newTestRtc(protocol.RtcStatusCompleted, "plain text answer", ""))
	if !strings.Contains(got, "plain text answer") {
		t.Errorf("expected opaque text preserved, got: %s", got)
	}
	if !strings.Contains(got, "You can now continue") {
		t.Errorf("missing footer on fallback: %s", got)
	}
}

func TestFormatAskUserResult_Failed(t *testing.T) {
	got := formatAskUserResult(newTestRtc(protocol.RtcStatusFailed, "boom", "execution blew up"))
	if !strings.Contains(got, "[Tool Error]") {
		t.Errorf("expected [Tool Error] prefix, got: %s", got)
	}
	if !strings.Contains(got, "ask_user") {
		t.Errorf("missing tool name: %s", got)
	}
	// ErrorMessage takes precedence over Result for failed status.
	if !strings.Contains(got, "execution blew up") {
		t.Errorf("expected ErrorMessage to be used, got: %s", got)
	}
}

func TestFormatAskUserResult_Timeout(t *testing.T) {
	got := formatAskUserResult(newTestRtc(protocol.RtcStatusTimeout, "", ""))
	if !strings.Contains(got, "[Tool Timeout]") {
		t.Errorf("expected [Tool Timeout], got: %s", got)
	}
}

func TestFormatAskUserResult_Rejected(t *testing.T) {
	got := formatAskUserResult(newTestRtc(protocol.RtcStatusRejected, "", ""))
	if !strings.Contains(got, "[Tool Rejected]") {
		t.Errorf("expected [Tool Rejected], got: %s", got)
	}
	if !strings.Contains(got, "rejected by user") {
		t.Errorf("expected 'rejected by user' message, got: %s", got)
	}
}

func TestFormatAskUserResult_Pending(t *testing.T) {
	got := formatAskUserResult(newTestRtc(protocol.RtcStatusPending, "", ""))
	if !strings.Contains(got, "[Tool Pending]") {
		t.Errorf("expected [Tool Pending], got: %s", got)
	}
}
