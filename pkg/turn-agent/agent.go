package turnagent

import (
	"context"
	"fmt"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// Agent is a stateful processor that manages turns for sessions.
// Each Process call delegates to a per-session SessionTurnManager via the
// SessionManagerRegistry. The registry creates managers on demand and reuses
// them when the session's loop is still running.
type Agent struct {
	cfg      Config
	queue    *rtcqueue.Queue
	workerID string
	registry *SessionManagerRegistry
}

// New constructs an Agent.
func New(cfg Config, queue *rtcqueue.Queue, workerID string) (*Agent, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("turnagent: %w", err)
	}
	if cfg.DeriveCheckpointID == nil {
		cfg.DeriveCheckpointID = func(sessionID string) string {
			return "turnagent:session:" + sessionID
		}
	}
	a := &Agent{
		cfg:      cfg,
		queue:    queue,
		workerID: workerID,
		registry: NewSessionManagerRegistry(),
	}
	a.logIfEnabled(context.Background(), LogLevelDebug, "agent.new", map[string]any{
		"has_logger":         cfg.Logger != nil,
		"has_tracer":         cfg.Tracer != nil,
		"has_metrics":        cfg.Metrics != nil,
		"enable_llm_logging": cfg.EnableLLMLogging,
		"has_callbacks":      len(cfg.Callbacks) > 0,
	})
	return a, nil
}
