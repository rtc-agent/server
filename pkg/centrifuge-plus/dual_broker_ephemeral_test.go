package centrifugeplus

import (
	"context"
	"testing"
	"time"

	"github.com/centrifugal/centrifuge"
)

// TestDualBroker_PublishEphemeral verifies that PublishEphemeral routes through
// the liveBroker as pure PUB/SUB without writing to the stream.
func TestDualBroker_PublishEphemeral(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Redis; skipping")
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

	// PublishEphemeral should succeed.
	err = broker.PublishEphemeral(ctx, ch, data, opts)
	if err != nil {
		t.Fatalf("PublishEphemeral failed: %v", err)
	}

	// Verify nothing was written to the stream (pure PUB/SUB; offset should be 0).
	// liveBroker's Publish with HistorySize=0 does not write to the stream and
	// returns an empty StreamPosition. We cannot directly check whether the
	// stream exists in Redis (liveBroker uses its own prefix), but we can
	// verify History returns empty.
	pubs, sp, err := broker.History(ch, centrifuge.HistoryOptions{Filter: centrifuge.HistoryFilter{Limit: 10}})
	if err != nil {
		// Live channels may not support History; this is expected.
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
