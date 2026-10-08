// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// RtcSessionUsecase handles RTC session and message management operations.
type RtcSessionUsecase struct {
	sessionRepo repo.SessionRepo
	messageRepo repo.MessageRepo
}

// NewRtcSessionUsecase creates a new RtcSessionUsecase.
func NewRtcSessionUsecase(sessionRepo repo.SessionRepo, messageRepo repo.MessageRepo) *RtcSessionUsecase {
	return &RtcSessionUsecase{
		sessionRepo: sessionRepo,
		messageRepo: messageRepo,
	}
}

// ListSessionsFilter holds filter criteria for admin session listing.
type ListSessionsFilter struct {
	UserID    string
	Status    string
	StartTime *time.Time
	EndTime   *time.Time
	Search    string
	Page      int
	PageSize  int
}

// ListSessionsResult holds the result of listing sessions.
type ListSessionsResult struct {
	Sessions []*model.Session
	Total    int64
	Page     int
	PageSize int
}

// ListSessions returns a paginated list of sessions with optional filters.
func (uc *RtcSessionUsecase) ListSessions(ctx context.Context, filter ListSessionsFilter) (*ListSessionsResult, error) {
	repoFilter := repo.SessionAdminFilter{
		UserID:    filter.UserID,
		Status:    filter.Status,
		StartTime: filter.StartTime,
		EndTime:   filter.EndTime,
		Search:    filter.Search,
		Page:      filter.Page,
		PageSize:  filter.PageSize,
	}

	sessions, total, err := uc.sessionRepo.ListForAdmin(ctx, repoFilter)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	return &ListSessionsResult{
		Sessions: sessions,
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}, nil
}

// ListMessagesFilter holds filter criteria for admin message listing.
type ListMessagesFilter struct {
	Role          string
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	Page          int
	PageSize      int
}

// ListMessagesResult holds the result of listing messages.
type ListMessagesResult struct {
	Messages []*model.Message
	Total    int64
	Page     int
	PageSize int
}

// ListMessages returns a paginated list of messages for a session.
func (uc *RtcSessionUsecase) ListMessages(ctx context.Context, sessionID uuid.UUID, filter ListMessagesFilter) (*ListMessagesResult, error) {
	// Verify session exists before querying messages.
	if _, err := uc.sessionRepo.GetByID(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	repoFilter := repo.MessageAdminFilter{
		Role:          filter.Role,
		CreatedAfter:  filter.CreatedAfter,
		CreatedBefore: filter.CreatedBefore,
		Page:          filter.Page,
		PageSize:      filter.PageSize,
	}

	messages, total, err := uc.messageRepo.ListForAdmin(ctx, sessionID, repoFilter)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	return &ListMessagesResult{
		Messages: messages,
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}, nil
}

// TokenStatDay holds daily aggregated token statistics (usecase-level type).
type TokenStatDay struct {
	Date              string `json:"date"`
	TotalTokens       int64  `json:"total_tokens"`
	TotalInputTokens  int64  `json:"total_input_tokens"`
	TotalOutputTokens int64  `json:"total_output_tokens"`
	CachedReadTokens  int64  `json:"total_cached_read_tokens"`
}

// GetTokenStatsResult holds the result of token statistics.
type GetTokenStatsResult struct {
	DailyStats  []*TokenStatDay
	Summary     *TokenStatsSummary
	TopSessions []*TopSessionInfo
}

// TokenStatsSummary holds aggregated token usage summary.
type TokenStatsSummary struct {
	TodayTokens int64 `json:"today_tokens"`
	WeekTokens  int64 `json:"week_tokens"`
	MonthTokens int64 `json:"month_tokens"`
	TotalTokens int64 `json:"total_tokens"`
}

// TopSessionInfo holds information about a top session by token usage.
type TopSessionInfo struct {
	SessionID   string    `json:"session_id"`
	Title       string    `json:"title"`
	TotalTokens int64     `json:"total_tokens"`
	CreatedAt   time.Time `json:"created_at"`
}

// GetTokenStats returns aggregated token statistics for a user.
func (uc *RtcSessionUsecase) GetTokenStats(ctx context.Context, userID string, days int) (*GetTokenStatsResult, error) {
	repoStats, err := uc.sessionRepo.AggregateTokenStats(ctx, userID, days)
	if err != nil {
		return nil, fmt.Errorf("get token stats: %w", err)
	}

	// Convert repo types to usecase types to avoid leaking repo layer details.
	dailyStats := make([]*TokenStatDay, 0, len(repoStats))
	for _, s := range repoStats {
		dailyStats = append(dailyStats, &TokenStatDay{
			Date:              s.Date,
			TotalTokens:       s.TotalTokens,
			TotalInputTokens:  s.TotalInputTokens,
			TotalOutputTokens: s.TotalOutputTokens,
			CachedReadTokens:  s.CachedReadTokens,
		})
	}

	topSessions, err := uc.ListTopSessions(ctx, userID, 10)
	if err != nil {
		return nil, fmt.Errorf("list top sessions: %w", err)
	}

	return &GetTokenStatsResult{
		DailyStats:  dailyStats,
		Summary:     computeTokenStatsSummary(dailyStats),
		TopSessions: topSessions,
	}, nil
}

// ListTopSessions returns the top sessions by token usage for a user.
func (uc *RtcSessionUsecase) ListTopSessions(ctx context.Context, userID string, limit int) ([]*TopSessionInfo, error) {
	sessions, err := uc.sessionRepo.ListTopByTokens(ctx, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list top sessions: %w", err)
	}

	result := make([]*TopSessionInfo, 0, len(sessions))
	for _, s := range sessions {
		result = append(result, &TopSessionInfo{
			SessionID:   s.ID.String(),
			Title:       s.Title,
			TotalTokens: s.TotalTokens,
			CreatedAt:   s.CreatedAt,
		})
	}
	return result, nil
}

// computeTokenStatsSummary computes summary from daily stats.
func computeTokenStatsSummary(dailyStats []*TokenStatDay) *TokenStatsSummary {
	now := time.Now()
	today := now.Format("2006-01-02")
	weekAgo := now.AddDate(0, 0, -6).Format("2006-01-02")
	monthAgo := now.AddDate(0, 0, -29).Format("2006-01-02")

	summary := &TokenStatsSummary{}
	for _, s := range dailyStats {
		summary.TotalTokens += s.TotalTokens
		if s.Date == today {
			summary.TodayTokens += s.TotalTokens
		}
		if s.Date >= weekAgo {
			summary.WeekTokens += s.TotalTokens
		}
		if s.Date >= monthAgo {
			summary.MonthTokens += s.TotalTokens
		}
	}
	return summary
}
