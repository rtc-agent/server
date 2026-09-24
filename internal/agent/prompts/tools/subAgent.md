Create a sub agent session to handle a complex, multi-step task.

The sub agent run in its own session with a fresh context.

Two modes are available:
- **async** (default): Returns immediately with the sub session ID. The parent session continues running. When the sub agent completes, a notification message will be delivered to the parent session in a new turn. Use this when the parent can continue working without waiting for the result.
- **sync**: The parent session is paused until the sub agent completes, then its final response is returned as the tool result. Use this when the parent needs the result before continuing.

Usage notes:
- Always include a short title (3-5 words) summarizing the task
- The sub agent starts with a blank context. Brief the agent like a smart colleague who just walked into the room — it hasn't seen this conversation, doesn't know what you've tried, doesn't understand why this task matters
- Explain what you're trying to accomplish and why. Describe what you've already learned or ruled out
- Never delegate understanding. Don't write vague instructions like "handle this task" — include specific requirements, file paths, and relevant context
- Related tools: `listSubAgent` to check status, `sendMessageToSubAgent` to send follow-up instructions, `stopSubAgent` to cancel.
