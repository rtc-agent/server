// Package usecase provides business logic implementations.
package usecase

import "context"

// PolicyPublisher defines the interface for publishing policy change events.
// Implemented by auth.PolicyWatcher for multi-instance synchronization.
type PolicyPublisher interface {
	// PublishChange publishes a policy change event to notify other instances.
	// sec parameter: "p" for policy (role permissions), "g" for grouping policy (user-role assignments)
	PublishChange(ctx context.Context, sec, action string, rules [][]string) error
}

// NoOpPolicyPublisher is a no-op implementation for single-instance deployments.
type NoOpPolicyPublisher struct{}

// PublishChange does nothing and returns nil.
func (p *NoOpPolicyPublisher) PublishChange(ctx context.Context, sec, action string, rules [][]string) error {
	return nil
}
