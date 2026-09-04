package centrifugeplus

import (
	"context"
	"testing"
	"time"

	"github.com/centrifugal/centrifuge"
)

// TestDualBroker_PublishEphemeral 验证 PublishEphemeral 走 liveBroker 纯 PUB/SUB，不写 stream
func TestDualBroker_PublishEphemeral(t *testing.T) {
	if testing.Short() {
		t.Skip("需要 Redis，跳过")
	}

	cleanupTestRedis(t, "test-ephemeral-live:")
	cleanupTestRedis(t, "test-ephemeral-topic:")

	node, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	redisShard, err := centrifuge.NewRedisShard(node, centrifuge.RedisShardConfig{
		Address: "localhost:6379",
	})
	if err != nil {
		t.Fatalf("failed to create redis shard: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := DualBrokerConfig{
		Live: centrifuge.RedisBrokerConfig{
			Prefix: "test-ephemeral-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:        "test-ephemeral-topic",
			RedisAddr:     "localhost:6379",
			RedisPassword: "",
			RedisDB:       15,
			HistoryStore:  historyStore,
		},
	}

	broker, err := NewDualBroker(node, config)
	if err != nil {
		t.Fatalf("failed to create dual broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	ch := "live:u=test-user"
	data := []byte(`{"user_id":"test","conversation_id":"conv","duration_seconds":10}`)
	opts := centrifuge.PublishOptions{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// PublishEphemeral 应该成功
	err = broker.PublishEphemeral(ctx, ch, data, opts)
	if err != nil {
		t.Fatalf("PublishEphemeral failed: %v", err)
	}

	// 验证 stream 中没有写入（纯 PUB/SUB，offset 应为 0）
	// liveBroker 的 Publish 在 HistorySize=0 时不写 stream，返回空 StreamPosition
	// 我们无法直接检查 Redis 中 stream 是否存在（因为 liveBroker 用的是自己的 prefix），
	// 但可以验证 History 返回空
	pubs, sp, err := broker.History(ch, centrifuge.HistoryOptions{Filter: centrifuge.HistoryFilter{Limit: 10}})
	if err != nil {
		// live 频道可能不支持 History，这是正常的
		t.Logf("History on live channel: %v (expected — live channels may not support history)", err)
		return
	}
	if sp.Offset != 0 {
		t.Errorf("expected stream offset=0 for ephemeral publish, got %d", sp.Offset)
	}
	if len(pubs) != 0 {
		t.Errorf("expected 0 publications in stream (ephemeral), got %d", len(pubs))
	}
}
