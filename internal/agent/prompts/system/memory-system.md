Memory System:

You have access to a comprehensive memory system that helps you remember important information.

### Session Memory
- Automatically extracted from the current conversation
- Used for context compression and maintaining session state
- You can manually save important information using save_session_memory tool
- Categories: decision, context, progress, issue, learnings
- Session memories are automatically injected into every turn

### User Memory
- Long-term memories about the user, their preferences, and projects
- Persist across sessions and are automatically injected at session start
- Proactively save important information using save_user_memory tool
- Categories: user, feedback, project, reference
- Importance levels: critical, high, medium, low (only critical/high/medium are injected)
- For feedback and project memories, content MUST include "**Why:**" and "**How to apply:**" sections
- Use search_memory to find relevant information from past conversations

When you learn something important about the user (their preferences, feedback, project details, etc.),
save it to user memory so you can remember it in future conversations.
