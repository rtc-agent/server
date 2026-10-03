package db

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// sharedPlugin is created once and reused across tests to avoid duplicate
// prometheus metric registration panics (promauto registers to global registry).
var sharedPlugin = NewMetricsPlugin()

// getCounterValue returns the current value of a counter with the given labels.
func getCounterValue(cv *MetricsPlugin, operation, status string) float64 {
	var m dto.Metric
	counter, err := cv.queriesTotal.GetMetricWithLabelValues(operation, status)
	if err != nil {
		return 0
	}
	if err := counter.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// openTestDB opens an in-memory SQLite database with the metrics plugin registered.
func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Use(sharedPlugin); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestMetricsPlugin_SuccessOperations tests that successful operations are counted.
func TestMetricsPlugin_SuccessOperations(t *testing.T) {
	db := openTestDB(t)

	type testModel struct {
		ID   uint   `gorm:"primarykey"`
		Name string `gorm:"size:100"`
	}

	// Auto migrate
	if err := db.AutoMigrate(&testModel{}); err != nil {
		t.Fatal(err)
	}

	// Record baseline counts
	insertBefore := getCounterValue(sharedPlugin, "insert", "success")
	selectBefore := getCounterValue(sharedPlugin, "select", "success")
	updateBefore := getCounterValue(sharedPlugin, "update", "success")
	deleteBefore := getCounterValue(sharedPlugin, "delete", "success")

	// Create
	db.Create(&testModel{Name: "test"})
	if v := getCounterValue(sharedPlugin, "insert", "success"); v <= insertBefore {
		t.Errorf("insert success count = %v, want > %v", v, insertBefore)
	}

	// Read
	var result testModel
	db.First(&result, "name = ?", "test")
	if v := getCounterValue(sharedPlugin, "select", "success"); v <= selectBefore {
		t.Errorf("select success count = %v, want > %v", v, selectBefore)
	}

	// Update
	db.Model(&result).Update("name", "updated")
	if v := getCounterValue(sharedPlugin, "update", "success"); v <= updateBefore {
		t.Errorf("update success count = %v, want > %v", v, updateBefore)
	}

	// Delete
	db.Delete(&result)
	if v := getCounterValue(sharedPlugin, "delete", "success"); v <= deleteBefore {
		t.Errorf("delete success count = %v, want > %v", v, deleteBefore)
	}
}

// TestMetricsPlugin_ErrorOperations tests that failed operations are counted with error status.
func TestMetricsPlugin_ErrorOperations(t *testing.T) {
	db := openTestDB(t)

	// Record baseline
	errorBefore := getCounterValue(sharedPlugin, "select", "error")

	// Attempt to query a non-existent table - should fail
	var result struct {
		ID   uint
		Name string
	}
	err := db.Table("non_existent_table_xyz").First(&result).Error
	if err == nil {
		t.Fatal("expected error querying non-existent table")
	}

	if v := getCounterValue(sharedPlugin, "select", "error"); v <= errorBefore {
		t.Errorf("select error count = %v, want > %v", v, errorBefore)
	}
}

// TestMetricsPlugin_ContextCancellation tests that context cancellation is not counted as error.
func TestMetricsPlugin_ContextCancellation(t *testing.T) {
	db := openTestDB(t)

	type testModel struct {
		ID   uint   `gorm:"primarykey"`
		Name string `gorm:"size:100"`
	}
	if err := db.AutoMigrate(&testModel{}); err != nil {
		t.Fatal(err)
	}

	// Record baseline
	errorBefore := getCounterValue(sharedPlugin, "select", "error")

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var result testModel
	_ = db.WithContext(ctx).First(&result).Error
	// The error might be context.Canceled or something else depending on timing.
	// We just verify the test doesn't crash and the error is not counted as DB error.

	// Context cancellation should not be counted as DB error
	errorAfter := getCounterValue(sharedPlugin, "select", "error")
	if errorAfter > errorBefore {
		t.Errorf("context cancellation counted as DB error: before=%v, after=%v", errorBefore, errorAfter)
	}
}

// TestMetricsPlugin_QueryDuration tests that query duration is recorded.
func TestMetricsPlugin_QueryDuration(t *testing.T) {
	db := openTestDB(t)

	type testModel struct {
		ID   uint   `gorm:"primarykey"`
		Name string `gorm:"size:100"`
	}
	if err := db.AutoMigrate(&testModel{}); err != nil {
		t.Fatal(err)
	}

	// Perform a query
	db.Create(&testModel{Name: "duration_test"})

	// Verify duration histogram has observations
	var m dto.Metric
	observer, err := sharedPlugin.queryDuration.GetMetricWithLabelValues("insert")
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.(prometheusMetric).Write(&m); err != nil {
		t.Fatal(err)
	}
	if m.GetHistogram().GetSampleCount() < 1 {
		t.Error("expected at least 1 histogram observation for insert")
	}
}

// prometheusMetric is an interface for metrics that can be written to a dto.Metric.
type prometheusMetric interface {
	Write(*dto.Metric) error
}

// TestMetricsPlugin_Name tests the plugin name.
func TestMetricsPlugin_Name(t *testing.T) {
	if name := sharedPlugin.Name(); name != "metrics_plugin" {
		t.Errorf("plugin name = %q, want %q", name, "metrics_plugin")
	}
}

// TestIsContextError tests the isContextError helper.
func TestIsContextError(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"nil context", nil, false},
		{"background context", context.Background(), false},
		{"cancelled context", func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}(), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isContextError(tt.ctx); got != tt.want {
				t.Errorf("isContextError() = %v, want %v", got, tt.want)
			}
		})
	}
}
