package webfetch

import "errors"

// Error types for classification and retry decisions.
var (
	ErrInvalidURL       = errors.New("invalid URL")
	ErrSSRFBlocked      = errors.New("SSRF check failed")
	ErrDomainBlocked    = errors.New("domain is blocked")
	ErrDNSFailed        = errors.New("DNS resolution failed")
	ErrTimeout          = errors.New("request timeout")
	ErrClientError      = errors.New("client error") // 4xx
	ErrServerError      = errors.New("server error") // 5xx
	ErrContentTooLarge  = errors.New("content too large")
	ErrTooManyRedirects = errors.New("too many redirects")
	ErrConversionFailed = errors.New("content conversion failed")
	ErrLLMFailed        = errors.New("LLM extraction failed")
	ErrConcurrencyLimit = errors.New("concurrency limit reached")
	ErrRateLimited      = errors.New("rate limited")
	ErrConnectionReset  = errors.New("connection reset")
	ErrRobotsBlocked    = errors.New("blocked by robots.txt")
)

// classifyError maps errors to metric label strings.
func classifyError(err error) string {
	switch {
	case errors.Is(err, ErrInvalidURL):
		return "invalid_url"
	case errors.Is(err, ErrSSRFBlocked):
		return "ssrf_blocked"
	case errors.Is(err, ErrDomainBlocked):
		return "domain_blocked"
	case errors.Is(err, ErrDNSFailed):
		return "dns_failed"
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrClientError):
		return "client_error"
	case errors.Is(err, ErrServerError):
		return "server_error"
	case errors.Is(err, ErrContentTooLarge):
		return "content_too_large"
	case errors.Is(err, ErrTooManyRedirects):
		return "too_many_redirects"
	case errors.Is(err, ErrConcurrencyLimit):
		return "concurrency_limit"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	default:
		return "unknown"
	}
}

// isRetryable returns whether the error is eligible for retry.
func isRetryable(err error) bool {
	return errors.Is(err, ErrTimeout) ||
		errors.Is(err, ErrServerError) ||
		errors.Is(err, ErrConnectionReset)
}
