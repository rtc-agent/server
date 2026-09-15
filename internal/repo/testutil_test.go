package repo

import (
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var testDBCounterUnified atomic.Int64

// setupTestDBWithModels creates a SQLite in-memory database and auto-migrates the given models.
// Each call uses a unique DSN prefix + atomic counter so parallel tests don't collide.
func setupTestDBWithModels(t *testing.T, prefix string, models ...interface{}) *gorm.DB {
	t.Helper()
	n := testDBCounterUnified.Add(1)
	dsn := fmt.Sprintf("file:%s%d?mode=memory&cache=shared", prefix, n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}
