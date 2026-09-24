# Write

Writes a file to the virtual filesystem.

## Usage

- This tool will overwrite the existing file if there is one at the provided path, or create a new file if it doesn't exist
- If this is an existing file, you MUST use the Read tool first to read the file's contents. This tool will fail if you did not read the file first
- Prefer the Edit tool for modifying existing files — it only sends the diff. Only use this tool to create new files or for complete rewrites
- Don't create files unless they're absolutely necessary for achieving your goal
- When writing code, include only what the task requires — don't add unnecessary boilerplate
