package repo

import (
	"context"

	"gorm.io/gorm"
)

// txContextKey is the private type used as context key for transactions.
type txContextKey struct{}

// WithTx injects a transaction object into the context.
// Repo methods automatically use the transaction via DBFromContext without
// requiring interface signature changes.
func WithTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

// TxFromContext extracts the transaction object from the context.
// Returns nil if no transaction is present.
func TxFromContext(ctx context.Context) *gorm.DB {
	tx, _ := ctx.Value(txContextKey{}).(*gorm.DB)
	return tx
}

// DBFromContext returns the transactional *gorm.DB if the context carries a tx,
// otherwise returns the fallback. All repos use this function uniformly,
// eliminating duplicated txOrDB methods across individual repos.
func DBFromContext(ctx context.Context, fallback *gorm.DB) *gorm.DB {
	if tx := TxFromContext(ctx); tx != nil {
		return tx
	}
	return fallback
}
