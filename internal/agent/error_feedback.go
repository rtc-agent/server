package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/google/uuid"
)

// =============================================================================
// insertErrorMessage — 创建并推送一条错误类型消息
// =============================================================================

// insertErrorMessage creates and publishes an error-type message for the given
// session. The message is associated with the specified turn (if any) and pushed
// to the user's Centrifuge channel so the frontend can render it.
//
// The message does NOT enter the LLM context (convertDBMessage's default branch
// skips error content type).
func (h *helpers) insertErrorMessage(
	ctx context.Context,
	sessionID uuid.UUID,
	turnID *uuid.UUID,
	category protocol.ErrorCategory,
	title string,
	message string,
	retryable bool,
	rawError string,
) error {
	if h.deps.UpdatePublisher == nil {
		return nil
	}

	// Rate limit check — skip if exceeded (still logged, just no user-visible message).
	if !h.checkErrorMessageRateLimit(ctx, sessionID) {
		h.logger.Info(ctx, "insertErrorMessage.rate_limited", map[string]any{
			"session_id": sessionID.String(),
			"category":   string(category),
		})
		return nil
	}

	// Build ErrorContent.
	content := protocol.ErrorContent{
		Category:  category,
		Title:     title,
		Message:   message,
		Retryable: retryable,
	}

	// Attach sanitized raw error if provided.
	sanitized := sanitizeRawError(rawError)
	if sanitized != "" {
		content.RawError = &sanitized
		showRaw := h.showRawErrors
		content.ShowRawError = &showRaw
	}

	contentData := protocol.ContentData{
		Type: protocol.ContentTypeError,
		Data: content,
	}

	// Look up session to construct the correct Centrifuge channel.
	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session for error message: %w", err)
	}
	ch := channel.UserTopic(session.OwnerRefID)

	// Transaction: create message + publish in one atomic operation.
	_, err = h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		msgs, createErr := primitives.BatchCreateMessages(txCtx, h.deps, sessionID, turnID, []primitives.MessageToCreate{{
			Role:    protocol.MessageRoleAssistant,
			Creator: usecase.SystemCreator{},
			Content: contentData,
			Status:  protocol.MessageStreamingCompleted,
		}})
		if createErr != nil {
			return nil, createErr
		}

		var allItems []protocol.UpdateItem
		for _, msg := range msgs {
			allItems = append(allItems, protocol.UpdateItem{
				Entity:   protocol.EntityMessage,
				Action:   protocol.ActionCreated,
				EntityId: protocol.UUID(msg.ID.String()),
			})
		}
		return []updates.UpdatePublishItem{{Channel: ch, Items: allItems}}, nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			h.logger.Info(ctx, "insertErrorMessage.push_after_commit", map[string]any{
				"error": err.Error(),
			})
		} else {
			return fmt.Errorf("insertErrorMessage: %w", err)
		}
	}

	// Audit log — only metadata, never the raw error content.
	h.logger.Info(ctx, "audit.error_message_created", map[string]any{
		"session_id": sessionID.String(),
		"category":   string(category),
		"title":      title,
		"retryable":  retryable,
	})

	return nil
}

// =============================================================================
// classifyError — 将 Go error 分类为用户友好的错误消息
// =============================================================================

// classifyError maps a Go error to an ErrorCategory and user-friendly text.
// Detection priority:
//  1. prompt-too-long → context (retryable)
//  2. anthropic.Error → by Type() (api/permission/context/timeout)
//  3. net.OpError / url.Error → network
//  4. context.DeadlineExceeded → timeout
//  5. default → system
func classifyError(err error) (category protocol.ErrorCategory, title, message string, retryable bool) {
	if err == nil {
		return protocol.ErrorCategorySystem, "系统错误", "发生未知错误，请重试。", false
	}

	// 1. Prompt too long — highest priority because it can also match as
	//    InvalidRequestError below.
	if turnagent.IsPromptTooLongError(err) {
		return protocol.ErrorCategoryContext,
			"上下文超出限制",
			"对话内容太长，系统正在自动压缩。请稍等片刻。",
			true
	}

	// 1b. Stream idle timeout — LLM stream stopped producing data.
	if turnagent.IsStreamIdleTimeout(err) {
		return protocol.ErrorCategoryTimeout,
			"响应超时",
			"AI 响应时间过长，已自动中断。请重新发送消息。",
			false
	}

	// 2. Anthropic API errors (uses shared.ErrorType* constants).
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Type() {
		case shared.ErrorTypeOverloadedError:
			return protocol.ErrorCategoryAPI,
				"服务过载",
				"AI 服务暂时过载，系统正在自动重试。",
				true
		case shared.ErrorTypeRateLimitError:
			return protocol.ErrorCategoryAPI,
				"请求频率限制",
				"当前使用量已达上限，系统正在等待后自动重试。",
				true
		case shared.ErrorTypeAuthenticationError:
			return protocol.ErrorCategoryPermission,
				"服务认证失败",
				"服务暂时无法完成认证，请联系管理员。",
				false
		case shared.ErrorTypePermissionError:
			return protocol.ErrorCategoryPermission,
				"权限不足",
				"当前账号权限不足，请联系管理员。",
				false
		case shared.ErrorTypeInvalidRequestError:
			return protocol.ErrorCategoryContext,
				"请求参数错误",
				"请求参数无效，请检查输入后重试。",
				false
		case shared.ErrorTypeTimeoutError:
			return protocol.ErrorCategoryTimeout,
				"请求超时",
				"AI 服务响应超时，系统正在自动重试。",
				true
		default:
			return protocol.ErrorCategoryAPI,
				"API 错误",
				"AI 服务返回错误，系统正在自动重试。",
				true
		}
	}

	// 3. Network errors (net.OpError, url.Error, etc.).
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return protocol.ErrorCategoryNetwork,
			"网络错误",
			"与 AI 服务的连接中断，系统正在自动重试。",
			true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return protocol.ErrorCategoryNetwork,
			"网络错误",
			"与 AI 服务的连接中断，系统正在自动重试。",
			true
	}

	// 4. Context deadline exceeded.
	if errors.Is(err, context.DeadlineExceeded) {
		return protocol.ErrorCategoryTimeout,
			"响应超时",
			"AI 响应时间过长，已自动中断。请重新发送消息。",
			false
	}

	// 5. Default — unknown system error.
	return protocol.ErrorCategorySystem,
		"系统错误",
		"发生未知错误，请重试。如果问题持续，请联系支持。",
		false
}

// =============================================================================
// sanitizeRawError — 12 项净化规则
// =============================================================================

// sanitizationRule defines a single regex-based sanitization rule.
type sanitizationRule struct {
    pattern     *regexp.Regexp
    replacement string
}

// Pre-compiled regular expressions and sanitization rules.
// Rules are applied sequentially in the order defined below.
//
// Sanitization order (applied sequentially):
//  1. Anthropic API keys (sk-ant-...)
//  2. Bearer tokens
//  3. Credential-bearing URIs (SQL, Redis, etc.)
//  4. AWS access keys
//  5. GitHub/GitLab/VCS tokens
//  6. PEM private key blocks
//  7. Generic password/secret parameters
//  8. Internal IP addresses and hostnames
//  9. Go stack traces
//  10. Email addresses
//  11. Phone numbers
//  12. File path usernames
//  13. Truncation to maxRawErrorLen (500 chars)
var (
    // 1. Anthropic API key: sk-ant-...
    reAPIKey = regexp.MustCompile(`sk-ant-[a-zA-Z0-9]+`)
    // 2. Bearer token
    reBearerToken = regexp.MustCompile(`Bearer [a-zA-Z0-9._-]+`)
    // 3. SQL/Redis URI with credentials: scheme://user:pass@host
    reCredURI = regexp.MustCompile(`\w+://[^:\s]+:[^@\s]+@`)
    // 4. AWS access key: AKIA followed by 16 uppercase alphanumeric chars
    reAWSKey = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
    // 5. GitHub/GitLab token: ghp_, glpat-, gho_, ghs_ prefixes
    reVCSToken = regexp.MustCompile(`(ghp_|glpat-|gho_|ghs_)[a-zA-Z0-9_]+`)
    // 6. PEM private key blocks
    rePEMKey = regexp.MustCompile(`-----BEGIN[A-Z ]+PRIVATE KEY-----[\s\S]*?-----END[A-Z ]+PRIVATE KEY-----`)
    // 7. Generic password/secret parameters (case-insensitive)
    rePassword = regexp.MustCompile(`(?i)(password|passwd|secret|api[_-]?key|token)\s*[=:]\s*\S+`)
    // 8. Internal IPs and hostnames
    reInternalAddr = regexp.MustCompile(`\b(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|[a-z0-9-]+\.internal)\b`)
    // 9. Go stack trace pattern
    reGoStackTrace = regexp.MustCompile(`goroutine \d+ \[[^\]]+\]:\n\s+[\w/.]+\.go:\d+`)
    // 10. Email addresses
    reEmail = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
    // 11. Phone numbers (international format)
    rePhone = regexp.MustCompile(`\+\d{1,3}[-.\s]?\(?\d{1,4}\)?[-.\s]?\d{1,4}[-.\s]?\d{1,9}`)
    // 12. File paths with username: /home/username/... -> /home/[USER]/...
    reFilePath = regexp.MustCompile(`(/home/|/Users/|/usr/)[a-zA-Z0-9_.-]+/`)

    // sanitizationRules is the ordered rule table applied by sanitizeRawError.
    // Order is security-critical: credentials first, then PII, then metadata.
    sanitizationRules = []sanitizationRule{
        {reAPIKey, "[REDACTED_API_KEY]"},
        {reBearerToken, "[REDACTED_TOKEN]"},
        {reCredURI, "[REDACTED_URI]"},
        {reAWSKey, "[REDACTED_AWS_KEY]"},
        {reVCSToken, "[REDACTED_VCS_TOKEN]"},
        {rePEMKey, "[REDACTED_PRIVATE_KEY]"},
        {rePassword, "${1}=[REDACTED]"},
        {reInternalAddr, "[INTERNAL_ADDR]"},
        {reGoStackTrace, "[REDACTED_STACK_TRACE]"},
        {reEmail, "[REDACTED_EMAIL]"},
        {rePhone, "[REDACTED_PHONE]"},
        {reFilePath, "${1}[USER]/"},
    }
)

const maxRawErrorLen = 500

// sanitizeRawError applies sanitization rules to remove sensitive data
// from raw error strings before storage. See plan §3.6.1 for details.
//
// Rules are applied sequentially from the sanitizationRules table:
//  1. API keys (Anthropic)
//  2. Bearer tokens
//  3. Credential URIs
//  4. AWS keys
//  5. VCS tokens (GitHub/GitLab)
//  6. Private keys (PEM)
//  7. Password parameters
//  8. Internal addresses
//  9. Stack traces
//  10. Emails
//  11. Phone numbers
//  12. File paths with usernames
//  13. Truncation to 500 characters
func sanitizeRawError(raw string) string {
    if raw == "" {
        return ""
    }

    s := raw
    for _, rule := range sanitizationRules {
        s = rule.pattern.ReplaceAllString(s, rule.replacement)
    }

    // Truncate to 500 characters.
    if len(s) > maxRawErrorLen {
        s = s[:maxRawErrorLen] + "..."
    }

    return s
}

// =============================================================================
// 错误消息速率限制器
// =============================================================================

const (
	// errorMsgRateLimitPerSession is the max error messages per session per hour.
	errorMsgRateLimitPerSession = 20
	// errorMsgRateLimitTTL is the TTL for rate limit keys.
	errorMsgRateLimitTTL = 1 * time.Hour
)

// checkErrorMessageRateLimit checks whether an error message can be created
// for the given session. Uses Redis INCR + EXPIRE for atomic counting.
// Returns true if allowed, false if rate limited.
// On Redis failure, degrades to allowing all (fail-open).
func (h *helpers) checkErrorMessageRateLimit(ctx context.Context, sessionID uuid.UUID) bool {
	if h.rdb == nil {
		return true
	}

	// Key format: error_msg_rate:{sessionID}:{hour}
	// Hour format: 2006010215 (YYYYMMDDHH)
	hour := time.Now().UTC().Truncate(time.Hour).Format("2006010215")
	key := cache.ErrorMessageRateLimit(sessionID.String(), hour)

	count, err := h.rdb.Incr(ctx, key).Result()
	if err != nil {
		// Redis failure → fail-open (allow all).
		return true
	}

	// Set TTL on first increment.
	if count == 1 {
		if err := h.rdb.Expire(ctx, key, errorMsgRateLimitTTL).Err(); err != nil {
			// EXPIRE failure means the key will persist indefinitely.
			// Log a warning for monitoring; this is rare but should be investigated.
			h.logger.Info(ctx, "checkErrorMessageRateLimit.expire_failed", map[string]any{
				"session_id": sessionID.String(),
				"key":        key,
				"error":      err.Error(),
			})
		}
	}

	return count <= errorMsgRateLimitPerSession
}
