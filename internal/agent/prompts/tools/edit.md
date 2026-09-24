# Edit

Performs exact string replacements in files within the virtual filesystem.

## Usage

- You must use the Read tool at least once before editing a file. This tool will error if you attempt an edit without reading the file first
- The `old_string` must exactly match the text in the file, including indentation and whitespace
- The edit will FAIL if `old_string` is not found in the file or matches multiple locations. Either provide a larger string with more surrounding context to make it unique, or use `replace_all` to change every instance
- Use `replace_all` for replacing and renaming strings across the file — useful for renaming variables, for instance
- Always prefer editing existing files over writing new ones
- When editing text from Read tool output, ensure you preserve the exact indentation as it appears after the line number prefix
