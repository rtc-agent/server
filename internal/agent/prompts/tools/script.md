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

## Parameters

Scripts can accept parameters via the `params` field. Access them in your code through the top-level `params` variable:

### Example: Parameterized script

```javascript
// Save a reusable script with params
const { startDate, endDate, format } = params;
console.log(`Processing data from ${startDate} to ${endDate}`);

// Use the parameters
const result = {
  range: `${startDate} → ${endDate}`,
  outputFormat: format || 'json'
};

return result;
```

### Run with parameters

```json
{
  "action": "run",
  "name": "analyze-data",
  "params": {
    "startDate": "2024-01-01",
    "endDate": "2024-12-31",
    "format": "csv"
  }
}
```

## Best Practices

- **Save frequently reused scripts**: If you find yourself running the same or similar code multiple times, save it with `action: "save"` and a descriptive `name`. This makes it reusable via `action: "run"` and avoids rewriting code.
- **Use descriptive names**: When saving scripts, use clear, kebab-case names (e.g., `analyze-sales-data`, `format-csv-output`) so they're easy to find and reuse later.
- **Prefer eval for one-off tasks**: For single-use code, use `action: "eval"` (default) without saving to keep the workspace clean.
- **Document parameters**: When saving scripts, include comments or description explaining what parameters the script expects.
