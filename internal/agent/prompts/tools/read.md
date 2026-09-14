# Read

Read file contents from the virtual filesystem.

## Usage Notes

- The file_path parameter must be an absolute path within the virtual filesystem
- By default, reads the entire file. For large files, consider reading specific sections if supported.
- Use this tool instead of script-based approaches (e.g., don't use script to read files)
- If the file does not exist, an error will be returned — this is expected behavior
- When you need to understand existing code or content before modifying it, always read the file first
