// internal/handler/rpc/action.prompt.go
package rpchandler

import (
	"fmt"
	"strings"

	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
)

// needsPromptMessage reports whether a prompt message should be created
// alongside the user message.
//
// Current rule: if the user_message carries scenarios, create a merged
// prompt message to persist the scenario content across turns.
func needsPromptMessage(cd protocol.ContentData) bool {
	if cd.Type != protocol.ContentTypeUserMessage {
		return false
	}
	umc, err := primitives.ParseUserMessageContent(cd.Data)
	if err != nil {
		return false
	}
	return umc.Scenarios != nil && len(*umc.Scenarios) > 0
}

// buildPromptContent builds a prompt ContentData from user message scenarios.
// All scenarios are merged into a single prompt message (see Decision 3).
//
// Returns an empty ContentData if no scenarios are present.
func buildPromptContent(cd protocol.ContentData) (protocol.ContentData, error) {
	umc, err := primitives.ParseUserMessageContent(cd.Data)
	if err != nil {
		return protocol.ContentData{}, fmt.Errorf("parse user message content: %w", err)
	}
	if umc.Scenarios == nil || len(*umc.Scenarios) == 0 {
		return protocol.ContentData{}, nil
	}

	// Merge all scenarios into a single prompt.
	var combinedPrompt strings.Builder
	var titles []string
	for _, s := range *umc.Scenarios {
		if s.FileContent == "" {
			continue // skip empty scenarios (consistent with injectScenarioPrompts)
		}
		fmt.Fprintf(&combinedPrompt, "# %s\n\n%s\n\n", s.Title, s.FileContent)
		titles = append(titles, s.Title)
	}
	if len(titles) == 0 {
		return protocol.ContentData{}, nil // no valid scenarios
	}

	title := strings.Join(titles, ", ")
	if len(title) > 100 {
		title = title[:97] + "..."
	}

	return primitives.PromptContentData(
		"scenarios",             // name
		title,                   // title
		combinedPrompt.String(), // prompt
	)
}
