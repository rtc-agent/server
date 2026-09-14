# Script

Execute JavaScript code in the browser environment. Provide either a file path or inline code (mutually exclusive).

## When to Use

- Data transformation or computation that can't be done with file tools alone
- Browser automation tasks (e.g., testing web pages, scraping content)
- When you need to process or analyze file contents programmatically

## When NOT to Use

- Simple file reading/writing — use the read/write tools instead
- Simple file searching — use grep/find instead
- Tasks that can be accomplished with dedicated tools

## Usage Notes

- The script runs in a browser environment with access to browser APIs
- Provide either `file` (path to a .js file) or `code` (inline JavaScript) — not both
- Output from console.log is captured and returned as the tool result
- Errors thrown during execution are returned as error messages
