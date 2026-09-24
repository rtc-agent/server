# Read

Reads a file from the virtual filesystem. You can access any file directly by using this tool.

## Usage

- The `path` parameter must be an absolute path within the virtual filesystem
- By default, reads up to 2000 lines starting from the beginning of the file
- You can optionally specify `offset` (line number, 1-indexed) and `limit` (number of lines) — especially useful for long files
- Results are returned with line numbers, making it easy to reference specific lines
- Use this tool instead of script-based approaches for reading files
- If the file does not exist, an error will be returned — this is expected behavior
- When you need to understand existing content before modifying it, always read the file first
- If you read a file that exists but has empty contents, you will receive a warning
