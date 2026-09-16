package turnagent

import (
	"context"
	"encoding/json"
	"fmt"
)

// skipErrorMessageKey is the context key for controlling error message insertion.
// Defined as an unexported struct type to guarantee uniqueness.
type skipErrorMessageKey struct{}

// WithSkipErrorMessage returns a context that signals failTurn to skip
// error message insertion. Used when the caller has already inserted an
// error message through another path (e.g., reactive compact "compressing"
// feedback, or stale turn scanner notification).
func WithSkipErrorMessage(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipErrorMessageKey{}, true)
}

// ShouldSkipErrorMessage checks whether the context carries the skip flag.
func ShouldSkipErrorMessage(ctx context.Context) bool {
	v, _ := ctx.Value(skipErrorMessageKey{}).(bool)
	return v
}

// MarshalSubmitPayload constructs a JSON payload for a kind="submit" work item.
// attempt is the reactive compact escalation level (0 = first attempt). It is
// stored in the payload so that the next Process invocation can escalate
// compression (L1 → L2 → L3) instead of repeating L1 forever.
func MarshalSubmitPayload(sessionID string, attempt int) []byte {
	p := WorkPayload{
		Kind:                   WorkKindSubmit,
		SessionID:              sessionID,
		ReactiveCompactAttempt: attempt,
	}
	data, err := json.Marshal(p)
	if err != nil {
		// WorkPayload contains only simple fields; marshaling should never fail.
		panic(fmt.Sprintf("turnagent: marshal submit payload: %v", err))
	}
	return data
}
