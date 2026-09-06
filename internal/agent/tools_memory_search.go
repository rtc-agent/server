package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// searchMemoryTool 搜索记忆（同时搜索 Session Memory 和 User Memory）
// 当前只实现 Session Memory 搜索，User Memory 在下一个 Phase 实现
type searchMemoryTool struct {
	helpers *helpers
}

func (h *helpers) createSearchMemoryTool() tool.InvokableTool {
	return &searchMemoryTool{helpers: h}
}

func (t *searchMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "search_memory",
		Desc: "Search across session memories and user memories. " +
			"Use this to find relevant information from past conversations or general knowledge. " +
			"User memories support both keyword and semantic (embedding-based) search.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {
				Type:     schema.String,
				Desc:     "Search query (keywords or natural language)",
				Required: true,
			},
			"category": {
				Type:     schema.String,
				Desc:     "Optional: filter by category (decision, context, progress, issue, learnings for session; user, feedback, project, reference for user)",
				Required: false,
			},
			"memory_type": {
				Type:     schema.String,
				Desc:     "Type of memory to search: 'session', 'user', or 'all' (default: 'all')",
				Required: false,
				Enum:     []string{"session", "user", "all"},
			},
			"limit": {
				Type:     schema.Integer,
				Desc:     "Optional: maximum number of results per memory type (default: 5)",
				Required: false,
			},
		}),
	}, nil
}

func (t *searchMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args struct {
		Query      string `json:"query"`
		Category   string `json:"category"`
		MemoryType string `json:"memory_type"`
		Limit      int    `json:"limit"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "search_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	if args.Query == "" {
		return "", fmt.Errorf("query is required")
	}

	// Set defaults
	if args.MemoryType == "" {
		args.MemoryType = "all"
	}
	if args.Limit <= 0 {
		args.Limit = 5
	}

	var results []searchResult

	// Search session memories
	if args.MemoryType == "all" || args.MemoryType == "session" {
		sessionResults, err := t.searchSessionMemories(ctx, args.Query, args.Category, args.Limit)
		if err != nil {
			t.helpers.logIfEnabled(ctx, "search_memory.session_error", map[string]any{
				"error": err.Error(),
			})
			// Continue with other searches even if session memory fails
		} else {
			results = append(results, sessionResults...)
		}
	}

	// Search user memories
	if args.MemoryType == "all" || args.MemoryType == "user" {
		userResults, err := t.searchUserMemories(ctx, args.Query, args.Category, args.Limit)
		if err != nil {
			t.helpers.logIfEnabled(ctx, "search_memory.user_error", map[string]any{
				"error": err.Error(),
			})
			// Continue with other searches even if user memory fails
		} else {
			results = append(results, userResults...)
		}
	}

	if len(results) == 0 {
		return "No memories found matching the query.", nil
	}

	// Format output
	var output strings.Builder
	fmt.Fprintf(&output, "Found %d memories:\n\n", len(results))

	for i, result := range results {
		fmt.Fprintf(&output, "%d. **[%s/%s]** %s\n",
			i+1, result.MemoryType, result.Category, result.Title)
		fmt.Fprintf(&output, "   %s\n", truncateString(result.Content, 300))
		if !result.CreatedAt.IsZero() {
			fmt.Fprintf(&output, "   Created: %s\n", result.CreatedAt.Format("2006-01-02 15:04"))
		}
		output.WriteString("\n")
	}

	return output.String(), nil
}

// searchResult 统一的搜索结果格式
type searchResult struct {
	MemoryType string    // "session" or "user"
	ID         string
	Category   string
	Title      string
	Content    string
	CreatedAt  time.Time
}

// searchSessionMemories 搜索 session memories（简单的关键词匹配）
// 注意：这是一个简单的实现，使用 category 过滤。
// 未来可以升级为使用 embedding 进行语义搜索。
func (t *searchMemoryTool) searchSessionMemories(
	ctx context.Context,
	query string,
	category string,
	limit int,
) ([]searchResult, error) {
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		return nil, nil // No session, no results
	}

	// Query memories (with optional category filter)
	var memories []*model.SessionMemory
	var err error
	if category != "" {
		memories, err = t.helpers.deps.SessionMemoryRepo.ListByCategory(ctx, sessionID, category, limit*2)
	} else {
		memories, err = t.helpers.deps.SessionMemoryRepo.ListBySession(ctx, sessionID, limit*2)
	}
	if err != nil {
		return nil, fmt.Errorf("list session memories: %w", err)
	}

	// Simple keyword filtering (case-insensitive)
	queryLower := strings.ToLower(query)
	var results []searchResult

	for _, mem := range memories {
		// Check if query matches title or content
		if strings.Contains(strings.ToLower(mem.Title), queryLower) ||
			strings.Contains(strings.ToLower(mem.Content), queryLower) {
			results = append(results, searchResult{
				MemoryType: "session",
				ID:         mem.ID.String(),
				Category:   mem.Category,
				Title:      mem.Title,
				Content:    mem.Content,
				CreatedAt:  mem.CreatedAt,
			})
		}

		// Stop when we have enough results
		if len(results) >= limit {
			break
		}
	}

	return results, nil
}

// searchUserMemories 搜索 user memories
// 使用 embedding 语义搜索 + 关键词搜索，通过 RRF 融合结果
func (t *searchMemoryTool) searchUserMemories(
	ctx context.Context,
	query string,
	category string,
	limit int,
) ([]searchResult, error) {
	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		return nil, err
	}

	// 收集两组结果，然后用 RRF 融合
	keywordResults, err := t.helpers.deps.UserMemoryRepo.SearchByKeyword(ctx, userID, query, limit*2)
	if err != nil {
		t.helpers.logIfEnabled(ctx, "search_memory.user_keyword_error", map[string]any{
			"error": err.Error(),
		})
	}

	var embeddingResults []*model.UserMemory
	if t.helpers.deps.EmbeddingService != nil && t.helpers.deps.EmbeddingService.Dimension() > 0 {
		vec, embedErr := t.helpers.deps.EmbeddingService.GenerateEmbedding(ctx, query)
		if embedErr == nil {
			embeddingResults, err = t.helpers.deps.UserMemoryRepo.SearchByEmbedding(ctx, userID, vec, limit*2)
			if err != nil {
				t.helpers.logIfEnabled(ctx, "search_memory.user_embedding_error", map[string]any{
					"error": err.Error(),
				})
			}
		} else {
			t.helpers.logIfEnabled(ctx, "search_memory.embedding_gen_error", map[string]any{
				"error": embedErr.Error(),
			})
		}
	}

	// RRF 融合: score = sum(1 / (k + rank_i)) * importance_weight
	// k = 60 (标准 RRF 参数)
	const rrfK = 60.0
	type rrfEntry struct {
		memory *model.UserMemory
		score  float64
	}
	scoreMap := make(map[string]*rrfEntry)

	// 关键词结果
	for rank, mem := range keywordResults {
		if category != "" && mem.Category != category {
			continue
		}
		key := mem.ID.String()
		if entry, ok := scoreMap[key]; ok {
			entry.score += 1.0 / (rrfK + float64(rank+1))
		} else {
			scoreMap[key] = &rrfEntry{
				memory: mem,
				score:  1.0 / (rrfK + float64(rank+1)),
			}
		}
	}

	// 向量结果
	for rank, mem := range embeddingResults {
		if category != "" && mem.Category != category {
			continue
		}
		key := mem.ID.String()
		if entry, ok := scoreMap[key]; ok {
			entry.score += 1.0 / (rrfK + float64(rank+1))
		} else {
			scoreMap[key] = &rrfEntry{
				memory: mem,
				score:  1.0 / (rrfK + float64(rank+1)),
			}
		}
	}

	// 应用重要性权重并排序
	entries := make([]*rrfEntry, 0, len(scoreMap))
	for _, entry := range scoreMap {
		entry.score *= model.ImportanceWeight(entry.memory.Importance)
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].score > entries[j].score
	})

	// 转换为统一结果
	var results []searchResult
	for i, entry := range entries {
		if i >= limit {
			break
		}
		mem := entry.memory
		// 异步更新访问计数（不阻塞搜索）
		go func(id uuid.UUID) {
			_ = t.helpers.deps.UserMemoryRepo.IncrementAccessCount(context.Background(), id)
		}(mem.ID)

		results = append(results, searchResult{
			MemoryType: "user",
			ID:         mem.ID.String(),
			Category:   mem.Category,
			Title:      mem.Title,
			Content:    mem.Content,
			CreatedAt:  mem.CreatedAt,
		})
	}

	return results, nil
}
