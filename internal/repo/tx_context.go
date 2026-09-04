package repo

import (
	"context"

	"gorm.io/gorm"
)

// contextKey Tx 在 context 中的 key，使用私有类型避免冲突。
type txContextKey struct{}

// WithTx 将事务对象注入 context。
// Repo 方法通过 DbFromContext 自动使用事务，无需修改接口签名。
func WithTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

// TxFromContext 从 context 中提取事务对象。
// 不存在时返回 nil。
func TxFromContext(ctx context.Context) *gorm.DB {
	tx, _ := ctx.Value(txContextKey{}).(*gorm.DB)
	return tx
}

// DBFromContext 返回事务内的 *gorm.DB（若 ctx 中携带 tx），否则返回 fallback。
// 所有 repo 统一使用此函数，消除各 repo 重复定义 txOrDB 方法。
func DBFromContext(ctx context.Context, fallback *gorm.DB) *gorm.DB {
	if tx := TxFromContext(ctx); tx != nil {
		return tx
	}
	return fallback
}
