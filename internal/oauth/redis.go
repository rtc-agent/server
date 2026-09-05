// Package oauth 提供 OAuth2 state（CSRF 防护）存储实现。
package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/cache"

	"github.com/redis/go-redis/v9"
)

// ErrStateNotFound state 不存在或已过期
var ErrStateNotFound = errors.New("state not found")

// RedisStore 基于 Redis 的 StateStore 实现（生产 / 分布式环境使用）。
//
// 存储结构：
//
//	key   = oauth2:state:{state}   （由 cache.OAuth2State 构造）
//	value = provider 名称
//	TTL   = 由调用方指定（通常 10 分钟）
//
// GetDel 通过 Lua 脚本原子地读取并删除，防止重放攻击。
type RedisStore struct {
	client redis.UniversalClient
}

// NewRedisStore 创建 Redis state 存储
func NewRedisStore(client redis.UniversalClient) *RedisStore {
	if client == nil {
		panic("statestore: redis client is nil")
	}
	return &RedisStore{client: client}
}

// Set 存储 state（带 TTL）
func (s *RedisStore) Set(ctx context.Context, state string, value string, ttl time.Duration) error {
	if state == "" {
		return errors.New("state key is empty")
	}
	key := cache.OAuth2State(state)
	return s.client.Set(ctx, key, value, ttl).Err()
}

// GetDel 原子地获取并删除 state；不存在或已过期时返回 ErrStateNotFound
func (s *RedisStore) GetDel(ctx context.Context, state string) (string, error) {
	if state == "" {
		return "", ErrStateNotFound
	}
	key := cache.OAuth2State(state)

	result, err := cache.GetDel.Run(ctx, s.client, []string{key}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrStateNotFound
		}
		return "", fmt.Errorf("redis GetDel state: %w", err)
	}

	str, ok := result.(string)
	if !ok {
		return "", ErrStateNotFound
	}
	return str, nil
}
