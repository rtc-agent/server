// Package lifecycle provides application lifecycle management.
// It uniformly manages component startup, shutdown, and health checks to ensure graceful shutdown.
package lifecycle

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// Component defines a lifecycle-manageable interface.
type Component interface {
	// Start starts the component. An error indicates startup failure.
	Start(ctx context.Context) error

	// Stop stops the component. It should gracefully release resources.
	Stop(ctx context.Context) error

	// HealthCheck checks the component's health. An error indicates unhealthy state.
	HealthCheck(ctx context.Context) error
}

// Manager uniformly manages the lifecycle of multiple Components.
// Components are started in registration order and stopped in reverse order.
type Manager struct {
	mu           sync.Mutex
	components   []namedComponent
	shutdownCh   chan struct{}
	wg           sync.WaitGroup
	started      bool
	ctx          context.Context // lifecycle context, cancelled on Stop
	cancel       context.CancelFunc
	shutdownOnce sync.Once // ensures Stop's destructive actions run exactly once
}

// namedComponent wraps a Component with a name for logging.
type namedComponent struct {
	name string
	c    Component
}

// NewManager creates a lifecycle manager.
func NewManager() *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		shutdownCh: make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Register registers a component. Must be called before Start.
func (m *Manager) Register(name string, c Component) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		panic("lifecycle.Manager: cannot register after Start")
	}

	m.components = append(m.components, namedComponent{name: name, c: c})
}

// Start starts all components in registration order.
// If any component fails to start, already-started components are stopped and an error is returned.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return fmt.Errorf("lifecycle.Manager: already started")
	}

	for i, nc := range m.components {
		if err := nc.c.Start(ctx); err != nil {
			logger.Error(ctx, "[lifecycle.Manager] start component failed",
				zap.String("component", nc.name),
				zap.Error(err))

			// Stop already-started components in reverse order.
			for j := i - 1; j >= 0; j-- {
				if stopErr := m.components[j].c.Stop(context.Background()); stopErr != nil {
					logger.Error(ctx, "[lifecycle.Manager] stop component during rollback",
						zap.String("component", m.components[j].name),
						zap.Error(stopErr))
				}
			}
			return fmt.Errorf("start component %q: %w", nc.name, err)
		}

		logger.Info(ctx, "[lifecycle.Manager] component started",
			zap.String("component", nc.name))
	}

	m.started = true
	return nil
}

// Stop stops all components in reverse order.
// Even if a component fails to stop, other components continue to be stopped.
// Waits for all goroutines launched via Go to finish, or until context timeout.
// Safe for concurrent use — only the first caller performs the shutdown;
// subsequent callers block until shutdown completes.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	// Mark as stopped immediately so concurrent callers skip the shutdown sequence.
	m.started = false
	m.mu.Unlock()

	// Use sync.Once to ensure destructive actions (channel close, context cancel,
	// component shutdown) run exactly once, even under concurrent Stop() calls.
	m.shutdownOnce.Do(func() {
		close(m.shutdownCh)

		// Cancel the lifecycle context to signal all Go()-launched goroutines to exit.
		if m.cancel != nil {
			m.cancel()
		}

		// Stop components in reverse order.
		for i := len(m.components) - 1; i >= 0; i-- {
			nc := m.components[i]
			if err := nc.c.Stop(ctx); err != nil {
				logger.Error(ctx, "[lifecycle.Manager] stop component failed",
					zap.String("component", nc.name),
					zap.Error(err))
			} else {
				logger.Info(ctx, "[lifecycle.Manager] component stopped",
					zap.String("component", nc.name))
			}
		}
	})

	// Wait for all goroutines to finish (all callers wait, even concurrent ones).
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error(context.Background(), "[lifecycle.Manager] shutdown wait panic",
					zap.Any("recover", r),
					zap.String("stack", string(debug.Stack())))
			}
		}()
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		logger.Warn(ctx, "[lifecycle.Manager] shutdown timeout, goroutines still running")
		return ctx.Err()
	}
}

// HealthCheck checks the health of all components.
// Returns the first error found.
func (m *Manager) HealthCheck(ctx context.Context) error {
	m.mu.Lock()
	components := make([]namedComponent, len(m.components))
	copy(components, m.components)
	m.mu.Unlock()

	for _, nc := range components {
		if err := nc.c.HealthCheck(ctx); err != nil {
			return fmt.Errorf("health check failed for component %q: %w", nc.name, err)
		}
	}
	return nil
}

// Go launches a background goroutine and waits for it to finish on Stop.
// The goroutine should listen on ctx.Done() for graceful shutdown.
// name is used for log identification.
func (m *Manager) Go(name string, fn func(ctx context.Context)) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Error(context.Background(), "[lifecycle.Manager] goroutine panic",
					zap.String("name", name),
					zap.Any("recover", r))
			}
		}()

		fn(m.ctx)
	}()
}

// ShutdownCh returns a channel that is closed when Stop is called.
// Components can listen on this channel to detect shutdown signals early.
func (m *Manager) ShutdownCh() <-chan struct{} {
	return m.shutdownCh
}
