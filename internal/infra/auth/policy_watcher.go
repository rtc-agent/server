// Package auth provides Casbin enforcer integration for RBAC permission checking.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

const (
	// policyChannel is the Redis Pub/Sub channel for policy change notifications.
	policyChannel = "casbin:policy_changes"
)

// PolicyChange represents a policy change event published via Redis Pub/Sub.
type PolicyChange struct {
	Action   string     `json:"action"`    // "add", "remove", "add_batch", "remove_batch"
	Sec      string     `json:"sec"`       // "p" (policy) or "g" (grouping policy)
	Rules    [][]string `json:"rules"`     // affected rules
	SourceID string     `json:"source_id"` // instance ID that made the change
}

// PolicyWatcher synchronizes Casbin policies across multiple instances via Redis Pub/Sub.
//
// When a policy change occurs on one instance, it publishes the change to Redis.
// All other instances receive the notification and reload their enforcer's policy.
type PolicyWatcher struct {
	enforcer   *CasbinEnforcer
	rdb        *redis.Client
	sub        *redis.PubSub
	instanceID string
	mu         sync.Mutex
	closed     bool
}

// NewPolicyWatcher creates a PolicyWatcher that syncs policy changes across instances.
func NewPolicyWatcher(enforcer *CasbinEnforcer, rdb *redis.Client, instanceID string) (*PolicyWatcher, error) {
	if rdb == nil {
		return nil, fmt.Errorf("redis client is required for policy watcher")
	}

	pw := &PolicyWatcher{
		enforcer:   enforcer,
		rdb:        rdb,
		instanceID: instanceID,
	}

	// Subscribe to policy change channel
	pw.sub = rdb.Subscribe(context.Background(), policyChannel)
	_, err := pw.sub.Receive(context.Background())
	if err != nil {
		return nil, fmt.Errorf("subscribe to policy channel: %w", err)
	}

	// Start background listener
	go pw.listen()

	return pw, nil
}

// PublishChange publishes a policy change event to Redis.
// Called after local policy modifications to notify other instances.
// Implements usecase.PolicyPublisher interface.
// The sec parameter is now explicitly passed to avoid fragile string matching.
func (pw *PolicyWatcher) PublishChange(ctx context.Context, sec, action string, rules [][]string) error {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	if pw.closed {
		return nil
	}

	// Validate sec parameter
	if sec != "p" && sec != "g" {
		return fmt.Errorf("invalid sec parameter: must be 'p' (policy) or 'g' (grouping), got '%s'", sec)
	}

	change := PolicyChange{
		Action:   action,
		Sec:      sec,
		Rules:    rules,
		SourceID: pw.instanceID,
	}

	data, err := json.Marshal(change)
	if err != nil {
		return fmt.Errorf("marshal policy change: %w", err)
	}

	if err := pw.rdb.Publish(ctx, policyChannel, data).Err(); err != nil {
		return fmt.Errorf("publish policy change: %w", err)
	}
	return nil
}

// listen processes incoming policy change notifications.
func (pw *PolicyWatcher) listen() {
	ch := pw.sub.Channel()
	for msg := range ch {
		var change PolicyChange
		if err := json.Unmarshal([]byte(msg.Payload), &change); err != nil {
			logger.Warn(context.Background(), "policy_watcher.invalid_message",
				zap.Error(err), zap.String("payload", msg.Payload))
			continue
		}

		// Ignore our own changes
		if change.SourceID == pw.instanceID {
			continue
		}

		// Reload all policies from DB to ensure consistency
		// PERFORMANCE NOTE: LoadPolicy() reloads all policies on every change.
		// For high-concurrency scenarios with frequent policy changes, consider:
		// 1. Incremental updates (apply only the changed rules)
		// 2. Batching multiple changes before reload
		// 3. Using a more efficient sync mechanism (e.g., CRDTs)
		// Current approach prioritizes correctness over performance.
		pw.enforcer.mu.Lock()
		if err := pw.enforcer.enforcer.LoadPolicy(); err != nil {
			logger.Error(context.Background(), "policy_watcher.reload_failed",
				zap.Error(err), zap.String("source", change.SourceID))
		}
		pw.enforcer.mu.Unlock()

		logger.Info(context.Background(), "policy_watcher.policies_reloaded",
			zap.String("source", change.SourceID),
			zap.String("action", change.Action))
	}
}

// Close stops the policy watcher and unsubscribes from Redis.
func (pw *PolicyWatcher) Close() error {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	if pw.closed {
		return nil
	}
	pw.closed = true
	return pw.sub.Close()
}
