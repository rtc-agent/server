// Package token_usage provides aggregation queries for token usage statistics
// at User and System levels.
//
// User-level: aggregates all sessions owned by a specific user (including
// sub-agent descendant sessions, which inherit owner_ref_id).
//
// System-level: aggregates all sessions in the system (optionally filtered
// by time range).
package token_usage

import (
	"context"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"

	"gorm.io/gorm"
)

// Service Token usage aggregation service.
type Service struct {
	db *gorm.DB
}

// NewService creates a new token usage aggregation service.
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// UserTokenUsage Aggregated token usage for a single user.
type UserTokenUsage struct {
	UserID                 string  `json:"user_id"`
	TotalInputTokens       int64   `json:"total_input_tokens"`
	TotalOutputTokens      int64   `json:"total_output_tokens"`
	TotalTokens            int64   `json:"total_tokens"`
	TotalCachedReadTokens  int64   `json:"total_cached_read_tokens"`
	TotalCachedWriteTokens int64   `json:"total_cached_write_tokens"`
	TotalReasoningTokens   int64   `json:"total_reasoning_tokens"`
	TotalCostUSD           float64 `json:"total_cost_usd"`
	SessionCount           int     `json:"session_count"`
}

// GetUserUsage returns aggregated token usage for a specific user.
//
// Sub-agent descendant sessions are naturally included because they inherit
// the parent session's owner_ref_id (see tools_sub_agent.go).
// startTime and endTime filter by last_token_update_at (nullable).
func (s *Service) GetUserUsage(ctx context.Context, userID string, startTime, endTime *time.Time) (*UserTokenUsage, error) {
	var result UserTokenUsage

	query := repo.DBFromContext(ctx, s.db).WithContext(ctx).
		Model(&model.Session{}).
		Select(`
			owner_ref_id AS user_id,
			COALESCE(SUM(total_input_tokens), 0) AS total_input_tokens,
			COALESCE(SUM(total_output_tokens), 0) AS total_output_tokens,
			COALESCE(SUM(total_tokens), 0) AS total_tokens,
			COALESCE(SUM(total_cached_read_tokens), 0) AS total_cached_read_tokens,
			COALESCE(SUM(total_cached_write_tokens), 0) AS total_cached_write_tokens,
			COALESCE(SUM(total_reasoning_tokens), 0) AS total_reasoning_tokens,
			COALESCE(SUM(total_cost_micros), 0) / 1000000.0 AS total_cost_usd,
			COUNT(*) AS session_count
		`).
		Where("owner_ref_id = ? AND owner_kind = ? AND deleted_at IS NULL", userID, "user")

	if startTime != nil {
		query = query.Where("last_token_update_at >= ?", *startTime)
	}
	if endTime != nil {
		query = query.Where("last_token_update_at <= ?", *endTime)
	}

	if err := query.Scan(&result).Error; err != nil {
		return nil, fmt.Errorf("get user token usage for %s: %w", userID, err)
	}
	result.UserID = userID
	return &result, nil
}

// SystemTokenUsage Aggregated token usage across all sessions.
type SystemTokenUsage struct {
	TotalInputTokens       int64   `json:"total_input_tokens"`
	TotalOutputTokens      int64   `json:"total_output_tokens"`
	TotalTokens            int64   `json:"total_tokens"`
	TotalCachedReadTokens  int64   `json:"total_cached_read_tokens"`
	TotalCachedWriteTokens int64   `json:"total_cached_write_tokens"`
	TotalReasoningTokens   int64   `json:"total_reasoning_tokens"`
	TotalCostUSD           float64 `json:"total_cost_usd"`
	ActiveSessions         int     `json:"active_sessions"`
	TotalSessions          int64   `json:"total_sessions"`
}

// GetSystemUsage returns aggregated token usage across all sessions.
// startTime and endTime filter by last_token_update_at (nullable).
//
// Note: The current session model does not have a tenant_id column.
// When multi-tenancy is added, a tenantID filter should be applied here.
func (s *Service) GetSystemUsage(ctx context.Context, startTime, endTime *time.Time) (*SystemTokenUsage, error) {
	var result SystemTokenUsage

	query := repo.DBFromContext(ctx, s.db).WithContext(ctx).
		Model(&model.Session{}).
		Select(`
			COALESCE(SUM(total_input_tokens), 0) AS total_input_tokens,
			COALESCE(SUM(total_output_tokens), 0) AS total_output_tokens,
			COALESCE(SUM(total_tokens), 0) AS total_tokens,
			COALESCE(SUM(total_cached_read_tokens), 0) AS total_cached_read_tokens,
			COALESCE(SUM(total_cached_write_tokens), 0) AS total_cached_write_tokens,
			COALESCE(SUM(total_reasoning_tokens), 0) AS total_reasoning_tokens,
			COALESCE(SUM(total_cost_micros), 0) / 1000000.0 AS total_cost_usd,
			COUNT(CASE WHEN status = ? THEN 1 END) AS active_sessions,
			COUNT(*) AS total_sessions
		`).
		Where("deleted_at IS NULL")

	if startTime != nil {
		query = query.Where("last_token_update_at >= ?", *startTime)
	}
	if endTime != nil {
		query = query.Where("last_token_update_at <= ?", *endTime)
	}

	if err := query.Scan(&result).Error; err != nil {
		return nil, fmt.Errorf("get system token usage: %w", err)
	}
	return &result, nil
}
