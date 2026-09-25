package websearch

import (
	"context"
	"errors"
	"math/rand"
	"sync/atomic"
)

// Balancer defines the load balancer interface
type Balancer interface {
	// Select chooses an available provider
	// Implementation should skip circuit-broken providers or let WebSearchManager filter them
	Select(ctx context.Context, available []WebSearchProvider) (WebSearchProvider, error)

	// Name returns strategy name (used for logging and metrics)
	Name() string
}

// roundRobinBalancer implements simple round-robin selection
type roundRobinBalancer struct {
	current atomic.Uint64
}

// NewRoundRobinBalancer creates a round-robin load balancer
func NewRoundRobinBalancer() Balancer {
	return &roundRobinBalancer{}
}

func (b *roundRobinBalancer) Select(ctx context.Context, available []WebSearchProvider) (WebSearchProvider, error) {
	if len(available) == 0 {
		return nil, errors.New("no available provider")
	}
	idx := b.current.Add(1) % uint64(len(available))
	return available[idx], nil
}

func (b *roundRobinBalancer) Name() string { return "round_robin" }

// weightedRandomBalancer implements weighted random selection
type weightedRandomBalancer struct {
	weights map[string]int // provider name -> weight
}

// NewWeightedRandomBalancer creates a weighted random load balancer
func NewWeightedRandomBalancer(weights map[string]int) Balancer {
	return &weightedRandomBalancer{weights: weights}
}

func (b *weightedRandomBalancer) Select(ctx context.Context, available []WebSearchProvider) (WebSearchProvider, error) {
	if len(available) == 0 {
		return nil, errors.New("no available provider")
	}

	totalWeight := 0
	for _, p := range available {
		if w, ok := b.weights[p.Name()]; ok {
			totalWeight += w
		} else {
			totalWeight += 1 // default weight
		}
	}

	r := rand.Intn(totalWeight)
	for _, p := range available {
		w := 1
		if v, ok := b.weights[p.Name()]; ok {
			w = v
		}
		r -= w
		if r < 0 {
			return p, nil
		}
	}
	return nil, errors.New("weighted selection failed")
}

func (b *weightedRandomBalancer) Name() string { return "weighted_random" }