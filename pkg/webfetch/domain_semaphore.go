package webfetch

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// DomainSemaphore — per-domain concurrency control with periodic cleanup.
// ---------------------------------------------------------------------------

// DomainSemaphore manages per-domain concurrency slots.
type DomainSemaphore struct {
	mu           sync.Mutex
	domains      map[string]chan struct{}
	maxPerDomain int
}

// NewDomainSemaphore creates a new DomainSemaphore.
func NewDomainSemaphore(maxPerDomain int) *DomainSemaphore {
	return &DomainSemaphore{
		domains:      make(map[string]chan struct{}),
		maxPerDomain: maxPerDomain,
	}
}

// Acquire obtains a domain-level slot, blocking until available or ctx canceled.
func (ds *DomainSemaphore) Acquire(ctx context.Context, domain string) error {
	ds.mu.Lock()
	sem, ok := ds.domains[domain]
	if !ok {
		sem = make(chan struct{}, ds.maxPerDomain)
		ds.domains[domain] = sem
	}
	ds.mu.Unlock()

	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("context canceled while waiting for domain slot: %w", ctx.Err())
	}
}

// Release frees a domain-level slot.
func (ds *DomainSemaphore) Release(domain string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if sem, ok := ds.domains[domain]; ok {
		select {
		case <-sem:
		default:
			// prevent double-release
		}
	}
}

// cleanupLoop periodically removes idle domain entries to prevent unbounded growth.
func (ds *DomainSemaphore) cleanupLoop(ctx context.Context, shutdownCh <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ds.cleanup()
		case <-shutdownCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// cleanup removes all idle domain semaphore entries.
func (ds *DomainSemaphore) cleanup() {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	for domain, sem := range ds.domains {
		if len(sem) == 0 {
			delete(ds.domains, domain)
		}
	}
}
