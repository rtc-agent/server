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

//go:embed prompts/tools/edit.md
var editDesc string

//go:embed prompts/tools/grep.md
var grepDesc string

//go:embed prompts/tools/find.md
var findDesc string

//go:embed prompts/tools/script.md
var scriptDesc string

//go:embed prompts/tools/askUser.md
var askUserDesc string

//go:embed prompts/tools/todoWrite.md
var todoWriteDesc string

//go:embed prompts/tools/subAgent.md
var subAgentDesc string

//go:embed prompts/tools/listSubAgent.md
var listSubAgentDesc string

//go:embed prompts/tools/getSubAgentMessage.md
var getSubAgentMessageDesc string

//go:embed prompts/tools/stopSubAgent.md
var stopSubAgentDesc string

//go:embed prompts/tools/createGoal.md
var createGoalDesc string

//go:embed prompts/tools/completeGoal.md
var completeGoalDesc string

//go:embed prompts/tools/cancelGoal.md
var cancelGoalDesc string

//go:embed prompts/tools/createLoop.md
var createLoopDesc string

//go:embed prompts/tools/cancelLoop.md
var cancelLoopDesc string

//go:embed prompts/tools/completeLoop.md
var completeLoopDesc string

//go:embed prompts/tools/listLoops.md
var listLoopsDesc string

//go:embed prompts/tools/pauseLoop.md
var pauseLoopDesc string

//go:embed prompts/tools/resumeLoop.md
var resumeLoopDesc string

//go:embed prompts/tools/saveSessionMemory.md
var saveSessionMemoryDesc string

//go:embed prompts/tools/listSessionMemories.md
var listSessionMemoriesDesc string

//go:embed prompts/tools/saveUserMemory.md
var saveUserMemoryDesc string

//go:embed prompts/tools/updateUserMemory.md
var updateUserMemoryDesc string

//go:embed prompts/tools/deleteUserMemory.md
var deleteUserMemoryDesc string

//go:embed prompts/tools/listUserMemory.md
var listUserMemoryDesc string

//go:embed prompts/tools/searchMemory.md
var searchMemoryDesc string

//go:embed prompts/tools/sendMessageToSubAgent.md
var sendMessageToSubAgentDesc string
