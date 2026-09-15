// Package lifecycle 提供应用生命周期管理能力。
// 统一管理组件的启动、停止和健康检查，确保优雅关闭。
package lifecycle

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// Component 定义可生命周期管理的组件接口。
type Component interface {
	// Start 启动组件。返回错误表示启动失败。
	Start(ctx context.Context) error

	// Stop 停止组件。应优雅地释放资源。
	Stop(ctx context.Context) error

	// HealthCheck 检查组件健康状态。返回错误表示不健康。
	HealthCheck(ctx context.Context) error
}

// Manager 统一管理多个 Component 的生命周期。
// 组件按注册顺序启动，按逆序停止。
type Manager struct {
	mu         sync.Mutex
	components []namedComponent
	shutdownCh chan struct{}
	wg         sync.WaitGroup
	started    bool
	ctx        context.Context // 生命周期 context，Stop 时取消
	cancel     context.CancelFunc
}

// namedComponent 包装 Component 并记录名称，用于日志。
type namedComponent struct {
	name string
	c    Component
}

// NewManager 创建生命周期管理器。
func NewManager() *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		shutdownCh: make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Register 注册组件。必须在 Start 之前调用。
func (m *Manager) Register(name string, c Component) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		panic("lifecycle.Manager: cannot register after Start")
	}

	m.components = append(m.components, namedComponent{name: name, c: c})
}

// Start 按注册顺序启动所有组件。
// 任一组件启动失败时，会停止已启动的组件并返回错误。
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

			// 逆序停止已启动的组件
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

// Stop 按逆序停止所有组件。
// 即使某个组件停止失败，也会继续停止其他组件。
// 等待所有通过 Go 启动的 goroutine 结束，或 context 超时。
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	close(m.shutdownCh)

	// 取消生命周期 context，通知所有 Go() 启动的 goroutine 退出
	if m.cancel != nil {
		m.cancel()
	}

	// 逆序停止组件
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

	// 等待所有 goroutine 结束
	done := make(chan struct{})
	go func() {
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

// HealthCheck 检查所有组件的健康状态。
// 返回第一个发现的错误。
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

// Go 启动一个后台 goroutine，并在 Stop 时等待其结束。
// goroutine 内部应监听 ctx.Done() 以优雅退出。
// name 用于日志标识。
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

// ShutdownCh 返回一个 channel，在 Stop 调用时关闭。
// 组件可监听此 channel 以提前感知关闭信号。
func (m *Manager) ShutdownCh() <-chan struct{} {
	return m.shutdownCh
}
