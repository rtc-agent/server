package webfetch

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RobotsChecker checks robots.txt rules for URLs.
// Results are cached per-domain with a configurable TTL.
type RobotsChecker struct {
	cache    map[string]*robotsEntry
	cacheMu  sync.RWMutex
	cacheTTL time.Duration
	client   *http.Client
	logger   *zap.Logger
}

// robotsEntry holds a cached robots.txt result for a domain.
type robotsEntry struct {
	allowed   map[string]bool // path -> allowed (simplified)
	fetchedAt time.Time
	allowAll  bool // true if robots.txt is absent or parse failed (lenient)
}

// NewRobotsChecker creates a RobotsChecker with a dedicated HTTP client.
// The client uses a 5-second timeout to avoid blocking normal requests.
func NewRobotsChecker(cacheTTL time.Duration, logger *zap.Logger) *RobotsChecker {
	if cacheTTL == 0 {
		cacheTTL = 24 * time.Hour
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RobotsChecker{
		cache:    make(map[string]*robotsEntry),
		cacheTTL: cacheTTL,
		client:   &http.Client{Timeout: 5 * time.Second},
		logger:   logger,
	}
}

// IsAllowed checks whether the given URL path is allowed by robots.txt.
// Uses lenient policy: if robots.txt cannot be fetched or parsed, access is allowed.
func (r *RobotsChecker) IsAllowed(rawURL string, userAgent string) (bool, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false, err
	}

	host := parsed.Host
	r.cacheMu.RLock()
	entry, ok := r.cache[host]
	r.cacheMu.RUnlock()

	if ok && time.Since(entry.fetchedAt) < r.cacheTTL {
		if entry.allowAll {
			return true, nil
		}
		return r.checkPath(entry, parsed.Path, userAgent), nil
	}

	// Fetch robots.txt.
	robotsURL := fmt.Sprintf("%s://%s/robots.txt", parsed.Scheme, parsed.Host)
	resp, err := r.client.Get(robotsURL)
	if err != nil {
		r.logger.Warn("failed to fetch robots.txt, allowing by default",
			zap.String("url", robotsURL), zap.Error(err))
		r.cacheEntry(host, &robotsEntry{allowAll: true, fetchedAt: time.Now()})
		return true, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// No robots.txt → allow all.
		r.cacheEntry(host, &robotsEntry{allowAll: true, fetchedAt: time.Now()})
		return true, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // 1MB limit
	if err != nil {
		r.logger.Warn("failed to read robots.txt, allowing by default",
			zap.String("url", robotsURL), zap.Error(err))
		r.cacheEntry(host, &robotsEntry{allowAll: true, fetchedAt: time.Now()})
		return true, nil
	}

	entry = r.parseRobotsTxt(string(body), userAgent)
	entry.fetchedAt = time.Now()
	r.cacheEntry(host, entry)

	if entry.allowAll {
		return true, nil
	}
	return r.checkPath(entry, parsed.Path, userAgent), nil
}

// parseRobotsTxt parses robots.txt content and returns an entry.
// This is a simplified parser that handles the most common directives.
func (r *RobotsChecker) parseRobotsTxt(content, userAgent string) *robotsEntry {
	lines := strings.Split(content, "\n")
	var currentGroupUserAgents []string
	var relevantRules []robotsRule
	inRelevantGroup := false

	userAgentLower := strings.ToLower(userAgent)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Remove comments.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		directive := strings.TrimSpace(strings.ToLower(parts[0]))
		value := strings.TrimSpace(parts[1])

		switch directive {
		case "user-agent":
			valueLower := strings.ToLower(value)
			if valueLower == "*" || strings.Contains(userAgentLower, valueLower) {
				inRelevantGroup = true
				currentGroupUserAgents = append(currentGroupUserAgents, valueLower)
			} else {
				inRelevantGroup = false
			}
		case "allow", "disallow":
			if inRelevantGroup && value != "" {
				relevantRules = append(relevantRules, robotsRule{
					allow: directive == "allow",
					path:  value,
				})
			}
		}
	}

	if len(relevantRules) == 0 {
		return &robotsEntry{allowAll: true}
	}

	allowed := make(map[string]bool, len(relevantRules))
	for _, rule := range relevantRules {
		allowed[rule.path] = rule.allow
	}
	return &robotsEntry{allowed: allowed}
}

// checkPath checks if a path is allowed based on cached robots rules.
func (r *RobotsChecker) checkPath(entry *robotsEntry, path, userAgent string) bool {
	if entry.allowAll {
		return true
	}
	// Check exact match first, then prefix matches (longest match wins).
	var bestMatch string
	var bestMatchAllowed bool = true
	for rulePath, allowed := range entry.allowed {
		if path == rulePath || strings.HasPrefix(path, rulePath) {
			if len(rulePath) > len(bestMatch) {
				bestMatch = rulePath
				bestMatchAllowed = allowed
			}
		}
	}
	if bestMatch == "" {
		return true // No matching rule → allow by default.
	}
	return bestMatchAllowed
}

func (r *RobotsChecker) cacheEntry(host string, entry *robotsEntry) {
	r.cacheMu.Lock()
	r.cache[host] = entry
	r.cacheMu.Unlock()
}

// robotsRule represents a single Allow/Disallow rule.
type robotsRule struct {
	allow bool
	path  string
}
