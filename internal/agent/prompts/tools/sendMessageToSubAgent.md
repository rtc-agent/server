Send a message to an async sub-agent session as a user.

Use this to give additional instructions, provide new context, or redirect a running async sub-agent. The message is delivered as a user message and triggers a new turn in the sub-agent session.

Requirements:
- The target session must be an async sub-agent of the current session.
- The sub-agent session must not be closed.
