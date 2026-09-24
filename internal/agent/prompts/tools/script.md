# Script

Execute JavaScript code to perform **computation or data processing**.

## Core Purpose

Script performs **active computation** — it processes inputs, applies logic, and produces computed outputs.

### Script vs Content

**Script**: Code whose value is in **what it does**.

- Transforms data, computes results, makes decisions
- Contains functions, loops, conditionals
- Output is computed, not written

**Content**: Text whose value is in **what it says**.

- Documents, configuration, source code files
- Output is read by humans or systems

`script(save)` saves code that **computes** — not code that **composes text**.

**Script is NOT for**: Creating documents, configuration files, README files, or any text whose primary purpose is to be read by humans or systems rather than executed.

### Quick Decision Guide

| Scenario | Script? | Reason |
| -------- | ------- | ------ |
| Count lines across 50 files | Yes | Aggregation across inputs |
| Transform CSV to JSON | Yes | Data format transformation |
| Calculate statistics | Yes | Numeric computation |
| Validate form data | Yes | Data validation logic |
| Write a README document | No | Output is prose for reading |
| Create a config file | No | Output is configuration text |
| Generate API documentation | No | Output is documentation for humans |

## When to Use

Script is ideal when you need to:

- Transform or analyze data with custom logic
- Analyze or aggregate data across multiple files
- Process and validate form data before submission
- Implement data processing logic you'll reuse (e.g., calculations, transformations, validations)

## Usage Notes

- Runs in sandboxed environment with restricted APIs
- Provide either `file` (path) or `code` (inline) — not both
- Output via console.log is captured as result
- **Loop restrictions**: `while`, `do...while`, and `for(;;)` are not allowed. Use `for...of`, `for...in`, bounded `for` loops, or Array methods (forEach/map/filter/reduce) instead
- **Sandbox restrictions**: No network access (fetch, XMLHttpRequest), no direct file system access, no eval() or dynamic import(). Execution timeout applies
- **Available APIs**: Standard JavaScript built-ins (Array, Math, Date, JSON, Map, Set, RegExp, etc.), console.log for output, rtcAgent APIs for file operations

## Parameters

Scripts can accept parameters via the `params` field. Access them in your code through the top-level `params` variable:

### Example: Parameterized script with computation

```javascript
const { scores, weights } = params;
let total = 0;
let weightSum = 0;

for (const [i, score] of scores.entries()) {
    const w = weights[i] || 1;
    total += score * w;
    weightSum += w;
}

return {
    weightedAverage: total / weightSum,
    count: scores.length
};
```

### Run with parameters

```json
{
  "action": "run",
  "name": "calc-weighted-avg",
  "params": {
    "scores": [85, 90, 78],
    "weights": [0.3, 0.5, 0.2]
  }
}
```

## Best Practices

- **Include real computation**: Scripts should process data, not compose strings
- **Save reusable logic**: Use `action: "save"` for computation you'll run multiple times
- **Descriptive names**: Use kebab-case for script names (e.g., `analyze-sales`, `process-csv`)
- **Prefer eval for one-offs**: Use `action: "eval"` for single-use code
