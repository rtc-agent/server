# Web Search Tool

Search the web for current information and return results with source links.

## Capabilities

- Searches the web and returns formatted search results with titles, URLs, and snippets
- Provides up-to-date information beyond your knowledge cutoff
- Supports time-based filtering to find recent or historical content
- Returns results from multiple search engines with automatic failover

## CRITICAL REQUIREMENT - Sources Section

After answering the user's question, you MUST include a "Sources:" section at the end of your response, listing all relevant URLs as markdown hyperlinks:

    [Your answer here]

    Sources:
    - [Source Title 1](https://example.com/1)
    - [Source Title 2](https://example.com/2)

This is MANDATORY - never skip including sources in your response.

## Parameters

- `query` (required): The search query. Should be specific and clear. Examples: "Go 1.22 new features", "React 19 migration guide 2026"
- `max_results` (optional): Maximum number of results to return. Default: 10, Max: 30. Use fewer for quick answers, more for comprehensive research.
- `time_range` (optional): Limit results to a specific time period. Options: 'day' (last 24h), 'week', 'month', 'year'. Leave empty for all time.

## IMPORTANT - Use the Correct Year in Search Queries

- When searching for recent information, documentation, or current events, include the current year in your query
- Example: If the user asks for "latest Go docs", search for "Go programming language documentation 2026", NOT just "Go documentation"
- This ensures you get the most up-to-date results

## Usage Notes

- Use specific, clear queries for better results
- Use `max_results` to balance between quick answers (5-10) and comprehensive research (20-30)
- Use `time_range` when you need recent information (e.g., 'week' for latest news, 'year' for recent documentation)
- Results include titles, URLs, and snippets - analyze them to find the most relevant information

## When to Use

- Finding recent news, events, or announcements
- Looking up current facts, statistics, or documentation
- Researching topics that change over time
- Verifying information that may be outdated
- Accessing information beyond your knowledge cutoff

## When NOT to Use

- Historical facts that don't change
- Information already in your knowledge base
- Personal or private information
- When the search snippet already answers the question (no need to fetch full page)

## After Searching

- Analyze the search results and select the most relevant URLs
- If snippets are sufficient to answer the question, you can respond directly
- If you need deeper information from a specific URL, use the web_fetch tool (if available)
- Always cite your sources using markdown hyperlinks in the Sources section
