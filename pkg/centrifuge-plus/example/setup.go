package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/redis/rueidis"
)

const (
	redisAddr = "localhost:6379"
	serverURL = "ws://localhost:9090/connection/websocket"
	httpAddr  = ":9090"
)

func cleanupRedis(ctx context.Context, addr string) error {
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{addr},
	})
	if err != nil {
		return fmt.Errorf("failed to create redis client: %w", err)
	}
	defer client.Close()

	// FLUSHDB each database used by the example (0 for Live broker, 1 for Topic broker).
	for _, db := range []int64{0, 1} {
		if err := client.Do(ctx, client.B().Select().Index(db).Build()).Error(); err != nil {
			return fmt.Errorf("failed to select db %d: %w", db, err)
		}
		if err := client.Do(ctx, client.B().Flushdb().Build()).Error(); err != nil {
			return fmt.Errorf("failed to flush db %d: %w", db, err)
		}
	}
	return nil
}

func waitForServer(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("server at %s not ready within %v", addr, timeout)
}
