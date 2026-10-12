package logger

import (
	"context"
	"net/url"
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

// maxSQLLogLength is the maximum length of SQL statement to record in traces.
// SQL longer than this will be truncated to avoid performance overhead from sanitization.
// For very large batch INSERT statements, regex-based sanitization can be expensive.
const maxSQLLogLength = 10000

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

	// Record SQL (sanitized to prevent sensitive data leakage).
	// For very long SQL statements (e.g., batch INSERT), truncate first to avoid
	// performance overhead from regex-based sanitization.
	if db.Statement.SQL.String() != "" {
		sqlStr := db.Statement.SQL.String()
		if len(sqlStr) > maxSQLLogLength {
			// P3 optimization: Try to truncate at SQL keyword boundaries to preserve
			// more structure information for debugging. Fall back to fixed 500 chars
			// if no good truncation point is found.
			sqlStr = truncateSQLAtKeyword(sqlStr, 500)
		}
		span.SetAttributes(attribute.String("db.statement", sanitizeSQL(sqlStr)))
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
//
// Supported DSN formats:
//  1. URL format: postgres://user:pass@host:port/dbname?params
//     Note: Special characters in password must be URL-encoded (e.g., '/' -> '%2F')
//  2. Key-value format: host=... user=... dbname=... password=...
//     This format does not require URL encoding.
func extractDBNameFromDSN(db *gorm.DB) string {
	if db == nil || db.Config == nil || db.Dialector == nil {
		return ""
	}
	pgDialector, ok := db.Dialector.(postgres.Dialector)
	if !ok {
		return ""
	}
	dsn := pgDialector.DSN

	// Try URL format first: postgres://user:pass@host:port/dbname?params
	// Use net/url to correctly handle passwords containing '/' or other special characters
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err == nil && parsed.Path != "" {
			// Path format: /dbname - remove leading '/'
			if len(parsed.Path) > 1 {
				return parsed.Path[1:]
			}
		}
	}

	// Fallback to key-value format: host=... user=... dbname=... password=...
	for _, part := range strings.Fields(dsn) {
		if strings.HasPrefix(part, "dbname=") {
			return strings.TrimPrefix(part, "dbname=")
		}
	}
	return ""
}

// truncateSQLAtKeyword truncates a long SQL statement at a keyword boundary
// to preserve more structure information for debugging. It tries to find a
// good truncation point (VALUES, WHERE, SET) within the first 1000 characters.
// If no good point is found, it falls back to a fixed truncation at maxLen.
//
// Examples:
//
//	"INSERT INTO users VALUES (...500 more chars...)" ->
//	"INSERT INTO users VALUES ...(+500 chars)...(truncated)"
//
//	"UPDATE users SET name=... WHERE ...500 more chars..." ->
//	"UPDATE users SET ...(+500 chars)...(truncated)"
func truncateSQLAtKeyword(sqlStr string, maxLen int) string {
	if len(sqlStr) <= maxLen {
		return sqlStr
	}

	// Try to find a good truncation point within the first 1000 chars
	searchLimit := 1000
	if searchLimit > len(sqlStr) {
		searchLimit = len(sqlStr)
	}
	searchRegion := strings.ToUpper(sqlStr[:searchLimit])

	// Look for common SQL keywords that indicate a good truncation point
	keywords := []string{" VALUES ", " WHERE ", " SET ", " FROM "}
	for _, keyword := range keywords {
		if idx := strings.Index(searchRegion, keyword); idx > 0 && idx < maxLen {
			// Truncate right after the keyword
			return sqlStr[:idx+len(keyword)] + "...(truncated)"
		}
	}

	// Fallback: fixed truncation at maxLen
	return sqlStr[:maxLen] + "...(truncated)"
}

// Ensure TracingPlugin implements gorm.Plugin interface.
var _ gorm.Plugin = (*TracingPlugin)(nil)
