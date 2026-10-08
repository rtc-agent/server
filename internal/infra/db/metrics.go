// Package db provides a GORM plugin for Prometheus metrics collection.
// It intercepts all SQL operations (query, row, create, update, delete)
// and records count and duration metrics with operation and status labels.
package db

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"gorm.io/gorm"
)

// MetricsPlugin is a GORM plugin that records Prometheus metrics for all
// database operations. It captures query count and duration with labels
// for operation type (select/insert/update/delete) and status (success/error).
type MetricsPlugin struct {
	queriesTotal  *prometheus.CounterVec
	queryDuration *prometheus.HistogramVec
}

// NewMetricsPlugin creates a new DB metrics plugin with Prometheus metrics.
func NewMetricsPlugin() *MetricsPlugin {
	return &MetricsPlugin{
		queriesTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "db",
			Name:      "queries_total",
			Help:      "Total number of database queries executed via GORM.",
		}, []string{"operation", "status"}),
		queryDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "db",
			Name:      "query_duration_seconds",
			Help:      "Database query duration in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 13), // 1ms ~ 4.096s
		}, []string{"operation"}),
	}
}

// Name returns the plugin name.
func (p *MetricsPlugin) Name() string {
	return "metrics_plugin"
}

// settingsKey is the key used to store timing data in Statement.Settings.
const settingsKey = "db_metrics:start_time"

// Initialize registers the GORM callbacks for metrics collection.
func (p *MetricsPlugin) Initialize(db *gorm.DB) error {
	cb := &metricsCallbacks{plugin: p}

	// Register callbacks for each operation type.
	type callbackDef struct {
		registrar func(string, func(*gorm.DB)) error
		name      string
		fn        func(*gorm.DB)
	}

	callbacks := []callbackDef{
		// Query callbacks (SELECT)
		{db.Callback().Query().Before("gorm:query").Register, "metrics:before_query", cb.before},
		{db.Callback().Query().After("gorm:query").Register, "metrics:after_query", cb.after("select")},
		// Row callbacks (SELECT for single row)
		{db.Callback().Row().Before("gorm:row").Register, "metrics:before_row", cb.before},
		{db.Callback().Row().After("gorm:row").Register, "metrics:after_row", cb.after("select")},
		// Create callbacks (INSERT)
		{db.Callback().Create().Before("gorm:create").Register, "metrics:before_create", cb.before},
		{db.Callback().Create().After("gorm:create").Register, "metrics:after_create", cb.after("insert")},
		// Update callbacks (UPDATE)
		{db.Callback().Update().Before("gorm:update").Register, "metrics:before_update", cb.before},
		{db.Callback().Update().After("gorm:update").Register, "metrics:after_update", cb.after("update")},
		// Delete callbacks (DELETE)
		{db.Callback().Delete().Before("gorm:delete").Register, "metrics:before_delete", cb.before},
		{db.Callback().Delete().After("gorm:delete").Register, "metrics:after_delete", cb.after("delete")},
		// Raw callbacks (raw SQL)
		{db.Callback().Raw().Before("gorm:raw").Register, "metrics:before_raw", cb.before},
		{db.Callback().Raw().After("gorm:raw").Register, "metrics:after_raw", cb.after("raw")},
	}

	for _, c := range callbacks {
		if err := c.registrar(c.name, c.fn); err != nil {
			return err
		}
	}
	return nil
}

// metricsCallbacks holds the callbacks for metrics collection.
type metricsCallbacks struct {
	plugin *MetricsPlugin
}

// before records the start time of the operation.
func (cb *metricsCallbacks) before(db *gorm.DB) {
	db.Statement.Settings.Store(settingsKey, time.Now())
}

// after records the metrics for the completed operation.
func (cb *metricsCallbacks) after(operation string) func(*gorm.DB) {
	return func(db *gorm.DB) {
		status := "success"
		// Exclude context errors and ErrRecordNotFound from error status.
		// ErrRecordNotFound is normal control flow (e.g., .First() when record doesn't exist),
		// not a real database error.
		if db.Error != nil && !isContextError(db.Statement.Context) && !errors.Is(db.Error, gorm.ErrRecordNotFound) {
			status = "error"
		}

		cb.plugin.queriesTotal.WithLabelValues(operation, status).Inc()

		// Record duration if start time is available.
		if startTimeVal, ok := db.Statement.Settings.Load(settingsKey); ok {
			if startTime, ok := startTimeVal.(time.Time); ok {
				duration := time.Since(startTime).Seconds()
				if duration > 0 {
					cb.plugin.queryDuration.WithLabelValues(operation).Observe(duration)
				}
			}
		}
	}
}

// isContextError checks if the error is due to context cancellation.
// Context errors are not counted as DB errors since they represent client-side
// timeouts or cancellations, not database failures.
func isContextError(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	return ctx.Err() != nil
}

// Ensure MetricsPlugin implements gorm.Plugin interface.
var _ gorm.Plugin = (*MetricsPlugin)(nil)
