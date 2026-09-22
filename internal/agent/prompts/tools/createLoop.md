Create a recurring loop for the current session. The agent will execute the prompt at regular intervals.

Call this ONLY after:
1. Understanding the user's recurring task intent
2. Refining the prompt and parameters (interval, max_turns)
3. Proposing the plan to the user and receiving explicit confirmation

IMPORTANT: You MUST NOT call this tool if there is already an active loop. You MUST NOT call this tool if there is an active goal (they are mutually exclusive). You MUST NOT decide the parameters for the user — user confirmation is required.