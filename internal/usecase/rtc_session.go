// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/memory"
)

// RtcSessionUsecase handles RTC session and message management operations.
type RtcSessionUsecase struct {
	sessionRepo         repo.SessionRepo
	messageRepo         repo.MessageRepo
	turnRepo            repo.TurnRepo
	rtcRepo             repo.RtcRepo
	memoryRepo          memory.Repository
	scriptExecutionRepo repo.ScriptExecutionRepo
}

// NewRtcSessionUsecase creates a new RtcSessionUsecase.
func NewRtcSessionUsecase(
	sessionRepo repo.SessionRepo,
	messageRepo repo.MessageRepo,
	turnRepo repo.TurnRepo,
	rtcRepo repo.RtcRepo,
	memoryRepo memory.Repository,
	scriptExecutionRepo repo.ScriptExecutionRepo,
) *RtcSessionUsecase {
	return &RtcSessionUsecase{
		sessionRepo:         sessionRepo,
		messageRepo:         messageRepo,
		turnRepo:            turnRepo,
		rtcRepo:             rtcRepo,
		memoryRepo:          memoryRepo,
		scriptExecutionRepo: scriptExecutionRepo,
	}
}

// ============================================================
// Session operations (used by RPC handlers)
// ============================================================

// ErrPermissionDenied is returned when a user attempts to access a resource
// they do not own.
var ErrPermissionDenied = fmt.Errorf("permission denied")

// GetSession returns a session by ID.
// Returns nil and an error if the session is not found.
//
// WARNING: This method does NOT perform ownership validation. Use
// GetSessionOwned if you need to verify the session belongs to a specific user.
func (uc *RtcSessionUsecase) GetSession(ctx context.Context, id uuid.UUID) (*model.Session, error) {
	return uc.sessionRepo.GetByID(ctx, id)
}

// GetSessionOwned returns a session by ID after verifying ownership by the given user.
// Returns ErrPermissionDenied if the session does not belong to the user.
//
// This is the preferred method for RPC handlers that need to ensure users
// can only access their own sessions.
func (uc *RtcSessionUsecase) GetSessionOwned(ctx context.Context, id uuid.UUID, userID uuid.UUID) (*model.Session, error) {
	session, err := uc.sessionRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// Validate ownership: session must belong to the requesting user
	if session.OwnerKind != string(creatorKindUser) || session.OwnerRefID != userID.String() {
		return nil, ErrPermissionDenied
	}
	return session, nil
}

// creatorKindUser is the string representation of user creator kind.
// Defined here to avoid circular dependency with usecase package.
const creatorKindUser = "user"

// ListSessionsByUser returns sessions for a user with cursor pagination.
func (uc *RtcSessionUsecase) ListSessionsByUser(ctx context.Context, userID uuid.UUID, cursor *string, limit int) ([]*model.Session, error) {
	return uc.sessionRepo.GetByUser(ctx, userID, cursor, limit)
}

// UpdateSession updates specific fields of a session.
func (uc *RtcSessionUsecase) UpdateSession(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	return uc.sessionRepo.Update(ctx, id, fields)
}

// ============================================================
// Message operations (used by RPC handlers)
// ============================================================

// GetMessage returns a message by ID.
// Returns nil and an error if the message is not found.
func (uc *RtcSessionUsecase) GetMessage(ctx context.Context, id uuid.UUID) (*model.Message, error) {
	return uc.messageRepo.GetByID(ctx, id)
}

// FindMessageByClientID returns a message by client-assigned ID.
// Returns nil if not found (not an error).
func (uc *RtcSessionUsecase) FindMessageByClientID(ctx context.Context, clientID string) (*model.Message, error) {
	return uc.messageRepo.FindByClientID(ctx, clientID)
}

// ListMessagesBySession returns messages for a session with cursor pagination (ASC by global_offset).
func (uc *RtcSessionUsecase) ListMessagesBySession(ctx context.Context, sessionID uuid.UUID, cursor *uint32, limit int) ([]*model.Message, error) {
	return uc.messageRepo.ListBySession(ctx, sessionID, cursor, limit)
}

// ListMessagesBeforeOffset returns messages with minOffset <= global_offset <= maxOffset.
// Pass minOffset=0 to omit the lower bound.
func (uc *RtcSessionUsecase) ListMessagesBeforeOffset(ctx context.Context, sessionID uuid.UUID, minOffset uint32, maxOffset uint32, limit int) ([]*model.Message, error) {
	return uc.messageRepo.ListBySessionBeforeOffset(ctx, sessionID, minOffset, maxOffset, limit)
}

// ============================================================
// Turn operations (used by RPC handlers)
// ============================================================

// GetTurn returns a turn by ID.
// Returns nil and an error if the turn is not found.
func (uc *RtcSessionUsecase) GetTurn(ctx context.Context, id uuid.UUID) (*model.Turn, error) {
	return uc.turnRepo.GetByID(ctx, id)
}

// ListTurnsBySession returns turns for a session with cursor pagination.
func (uc *RtcSessionUsecase) ListTurnsBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Turn, error) {
	return uc.turnRepo.ListBySession(ctx, sessionID, cursor, limit)
}

// FindActiveTurnsBySession returns all non-terminal turns for a session.
func (uc *RtcSessionUsecase) FindActiveTurnsBySession(ctx context.Context, sessionID uuid.UUID) ([]*model.Turn, error) {
	return uc.turnRepo.FindActiveBySession(ctx, sessionID)
}

// ============================================================
// Admin operations (used by HTTP admin handlers)
// ============================================================

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
//
// Note: This method assumes the session exists and is accessible to the caller.
// The caller should verify session ownership before calling this method if needed.
func (uc *RtcSessionUsecase) ListMessages(ctx context.Context, sessionID uuid.UUID, filter ListMessagesFilter) (*ListMessagesResult, error) {
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

// ============================================================
// RTC operations (used by RPC handlers)
// ============================================================

// GetRtcSession returns an RTC by ID.
// Returns nil and an error if the RTC is not found.
func (uc *RtcSessionUsecase) GetRtcSession(ctx context.Context, id uuid.UUID) (*model.Rtc, error) {
	return uc.rtcRepo.GetByID(ctx, id)
}

// UpdateRtcOutputMessageID updates the output message ID for an RTC.
func (uc *RtcSessionUsecase) UpdateRtcOutputMessageID(ctx context.Context, id uuid.UUID, outputMessageID uuid.UUID) error {
	return uc.rtcRepo.UpdateOutputMessageID(ctx, id, outputMessageID)
}

// ListRtcSessionsBySession returns RTCs for a session with cursor pagination (ASC by creation time).
func (uc *RtcSessionUsecase) ListRtcSessionsBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Rtc, error) {
	return uc.rtcRepo.ListBySession(ctx, sessionID, cursor, limit)
}

// ============================================================
// Memory operations (used by RPC handlers)
// ============================================================

// DeleteMemoryByScope deletes all memories for the given scope and scope ID.
func (uc *RtcSessionUsecase) DeleteMemoryByScope(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID) error {
	return uc.memoryRepo.DeleteByScope(ctx, scope, scopeID)
}

// ============================================================
// Script Execution operations (used by RPC handlers)
// ============================================================

// CreateScriptExecution stores a new script execution record.
func (uc *RtcSessionUsecase) CreateScriptExecution(ctx context.Context, exec *model.ScriptExecution) error {
	return uc.scriptExecutionRepo.Create(ctx, exec)
}

// ListRecentMessagesBySession returns the most recent N messages for a session.
// This is used by the title summarizer to generate session titles.
func (uc *RtcSessionUsecase) ListRecentMessagesBySession(ctx context.Context, sessionID uuid.UUID, limit int) ([]*model.Message, error) {
	return uc.messageRepo.ListRecentBySession(ctx, sessionID, limit)
}

// UpdateSessionTitle updates the title of a session.
// This is used by the title summarizer after generating a new title.
func (uc *RtcSessionUsecase) UpdateSessionTitle(ctx context.Context, sessionID uuid.UUID, title string) error {
	return uc.sessionRepo.Update(ctx, sessionID, map[string]any{"title": title})
}
