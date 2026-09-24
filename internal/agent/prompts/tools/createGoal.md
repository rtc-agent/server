Create a persistent goal for the current session. The agent will keep working across turns until the condition is met or the goal is cancelled.

Call this ONLY after:
1. Investigating the current state (run tests, inspect files, etc.)
2. Refining the user's intent into a SMART condition (Specific, Measurable, Achievable, Relevant, Time-bound)
3. Proposing the condition to the user and receiving explicit confirmation

IMPORTANT: You MUST NOT call this tool if there is already an active goal. You MUST NOT decide the condition for the user — user confirmation is required.