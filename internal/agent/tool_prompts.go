// tool_prompts.go — Tool description templates.
//
// Each tool's description is stored in a separate Markdown file under
// prompts/tools/ and embedded at compile time. The descriptions are referenced
// directly by their variable names (e.g. lsDesc, readDesc) in tool registration
// code, keeping the text editable without touching Go source.
//
// Naming convention:
//   - Variables: <toolName>Desc (e.g. lsDesc, askUserDesc)
//   - Files: prompts/tools/<tool-name>.md (static, no template syntax)
package agent

import (
	_ "embed"
)

//go:embed prompts/tools/ls.md
var lsDesc string

//go:embed prompts/tools/read.md
var readDesc string

//go:embed prompts/tools/write.md
var writeDesc string

//go:embed prompts/tools/grep.md
var grepDesc string

//go:embed prompts/tools/find.md
var findDesc string

//go:embed prompts/tools/script.md
var scriptDesc string

//go:embed prompts/tools/ask-user.md
var askUserDesc string

//go:embed prompts/tools/todo-write.md
var todoWriteDesc string

//go:embed prompts/tools/sub-agent.md
var subAgentDesc string

//go:embed prompts/tools/list-sub-agent.md
var listSubAgentDesc string

//go:embed prompts/tools/get-sub-agent-message.md
var getSubAgentMessageDesc string

//go:embed prompts/tools/stop-sub-agent.md
var stopSubAgentDesc string

//go:embed prompts/tools/create-goal.md
var createGoalDesc string

//go:embed prompts/tools/complete-goal.md
var completeGoalDesc string

//go:embed prompts/tools/cancel-goal.md
var cancelGoalDesc string

//go:embed prompts/tools/create-loop.md
var createLoopDesc string

//go:embed prompts/tools/cancel-loop.md
var cancelLoopDesc string

//go:embed prompts/tools/complete-loop.md
var completeLoopDesc string

//go:embed prompts/tools/list-loops.md
var listLoopsDesc string

//go:embed prompts/tools/pause-loop.md
var pauseLoopDesc string

//go:embed prompts/tools/resume-loop.md
var resumeLoopDesc string

//go:embed prompts/tools/save-session-memory.md
var saveSessionMemoryDesc string

//go:embed prompts/tools/list-session-memories.md
var listSessionMemoriesDesc string

//go:embed prompts/tools/save-user-memory.md
var saveUserMemoryDesc string

//go:embed prompts/tools/update-user-memory.md
var updateUserMemoryDesc string

//go:embed prompts/tools/delete-user-memory.md
var deleteUserMemoryDesc string

//go:embed prompts/tools/list-user-memory.md
var listUserMemoryDesc string

//go:embed prompts/tools/search-memory.md
var searchMemoryDesc string

