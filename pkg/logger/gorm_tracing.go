package logger

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TracingPlugin is a GORM plugin that creates OpenTelemetry spans for SQL operations.
// It records SQL statements, affected rows, and errors as span attributes.
type TracingPlugin struct {
	tracerName string
	dbInstance string // database name for db.instance span attribute
}

// NewTracingPlugin creates a new GORM tracing plugin.
func NewTracingPlugin() *TracingPlugin {
	return &TracingPlugin{
		tracerName: "gorm",
	}
}

// Name returns the plugin name.
func (p *TracingPlugin) Name() string {
	return "tracing"
}

// Initialize registers the callbacks for tracing.
func (p *TracingPlugin) Initialize(db *gorm.DB) error {
	tracer := otel.Tracer(p.tracerName)

	// Extract database name from the postgres DSN for db.instance attribute.
	p.dbInstance = extractDBNameFromDSN(db)

	// Helper to extract or create context
	getCtx := func(db *gorm.DB) context.Context {
		if db.Statement.Context != nil {
			return db.Statement.Context
		}
		return context.Background()
	}

	// Create span before each operation, end it after
	cb := &tracingCallbacks{tracer: tracer, getCtx: getCtx, dbInstance: p.dbInstance}

	// Register callbacks for all CRUD operations
	if err := db.Callback().Create().Before("gorm:create").Register("tracing:before_create", cb.before("gorm.create")); err != nil {
		return err
	}
	if err := db.Callback().Create().After("gorm:create").Register("tracing:after_create", cb.after); err != nil {
		return err
	}

	if err := db.Callback().Query().Before("gorm:query").Register("tracing:before_query", cb.before("gorm.query")); err != nil {
		return err
	}
	if err := db.Callback().Query().After("gorm:query").Register("tracing:after_query", cb.after); err != nil {
		return err
	}

	if err := db.Callback().Update().Before("gorm:update").Register("tracing:before_update", cb.before("gorm.update")); err != nil {
		return err
	}
	if err := db.Callback().Update().After("gorm:update").Register("tracing:after_update", cb.after); err != nil {
		return err
	}

	if err := db.Callback().Delete().Before("gorm:delete").Register("tracing:before_delete", cb.before("gorm.delete")); err != nil {
		return err
	}
	if err := db.Callback().Delete().After("gorm:delete").Register("tracing:after_delete", cb.after); err != nil {
		return err
	}

	if err := db.Callback().Row().Before("gorm:row").Register("tracing:before_row", cb.before("gorm.row")); err != nil {
		return err
	}
	if err := db.Callback().Row().After("gorm:row").Register("tracing:after_row", cb.after); err != nil {
		return err
	}

	if err := db.Callback().Raw().Before("gorm:raw").Register("tracing:before_raw", cb.before("gorm.raw")); err != nil {
		return err
	}
	if err := db.Callback().Raw().After("gorm:raw").Register("tracing:after_raw", cb.after); err != nil {
		return err
	}

	return nil
}

type tracingCallbacks struct {
	tracer     trace.Tracer
	getCtx     func(*gorm.DB) context.Context
	dbInstance string // database name for db.instance span attribute
}

func (cb *tracingCallbacks) before(operation string) func(*gorm.DB) {
	return func(db *gorm.DB) {
		ctx := cb.getCtx(db)

		attrs := []attribute.KeyValue{
			attribute.String("db.system", "postgres"),
			attribute.String("db.operation", strings.TrimPrefix(operation, "gorm.")),
		}
		if cb.dbInstance != "" {
			attrs = append(attrs, attribute.String("db.instance", cb.dbInstance))
		}

		ctx, span := cb.tracer.Start(ctx, operation, trace.WithAttributes(attrs...))
		// Store span in Statement.Settings for retrieval in after callback
		db.Statement.Settings.Store("tracing:span", span)
		// Update context with span
		db.Statement.Context = ctx
	}
}

func (cb *tracingCallbacks) after(db *gorm.DB) {
	// Retrieve span from Statement.Settings
	spanVal, ok := db.Statement.Settings.Load("tracing:span")
	if !ok || spanVal == nil {
		return
	}
	span, ok := spanVal.(trace.Span)
	if !ok || span == nil {
		return
	}
	defer span.End()

	// Record SQL (sanitized to prevent sensitive data leakage)
	if db.Statement.SQL.String() != "" {
		span.SetAttributes(attribute.String("db.statement", sanitizeSQL(db.Statement.SQL.String())))
	}

	// Record affected rows
	if db.Statement.RowsAffected >= 0 {
		span.SetAttributes(attribute.Int64("db.rows_affected", db.Statement.RowsAffected))
	}

	// Record error if any (ignore ErrRecordNotFound — it's a normal control flow)
	if db.Error != nil && db.Error != gorm.ErrRecordNotFound {
		span.RecordError(db.Error)
		span.SetStatus(codes.Error, db.Error.Error())
	}
}

// extractDBNameFromDSN extracts the database name from the GORM dialector config.
// Returns empty string if the dialector is not postgres or the DSN cannot be parsed.
func extractDBNameFromDSN(db *gorm.DB) string {
	if db == nil || db.Config == nil || db.Dialector == nil {
		return ""
	}
	pgDialector, ok := db.Dialector.(postgres.Dialector)
	if !ok {
		return ""
	}
	dsn := pgDialector.DSN
	// Parse postgres DSN format: postgres://user:pass@host:port/dbname?params
	// or key-value format: host=... user=... dbname=... password=...
	if idx := strings.LastIndex(dsn, "/"); idx >= 0 {
		rest := dsn[idx+1:]
		if qIdx := strings.IndexByte(rest, '?'); qIdx >= 0 {
			return rest[:qIdx]
		}
		return rest
	}
	// Key-value format
	for _, part := range strings.Fields(dsn) {
		if strings.HasPrefix(part, "dbname=") {
			return strings.TrimPrefix(part, "dbname=")
		}
	}
	return ""
}

// Ensure TracingPlugin implements gorm.Plugin interface.
var _ gorm.Plugin = (*TracingPlugin)(nil)
