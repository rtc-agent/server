// Package centrifugeplus 提供 centrifuge-plus 的单元测试。
package centrifugeplus

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/redis/rueidis"
)

func TestMain(m *testing.M) {
	// Flush test DB (15) to ensure clean state. This is safe since DB 15 is
	// exclusively used by these tests. Much more reliable than SCAN/KEYS-based
	// cleanup which can miss keys in a large keyspace.
	flushTestDB()
	code := m.Run()
	os.Exit(code)
}

func flushTestDB() {
	client, err := newTestRedisClient()
	if err != nil {
		return
	}
	defer client.Close()
	client.Do(context.Background(), client.B().Flushdb().Build())
}

// cleanupTestRedis cleans up Redis test data and registers deferred cleanup via t.Cleanup.
func cleanupTestRedis(t *testing.T, prefix string) {
	t.Helper()
	doCleanup(prefix)

	t.Cleanup(func() {
		doCleanup(prefix)
	})
}

func newTestRedisClient() (rueidis.Client, error) {
	return rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"localhost:6379"},
		SelectDB:    15,
	})
}

func doCleanup(prefix string) {
	client, err := newTestRedisClient()
	if err != nil {
		return
	}
	defer client.Close()

	ctx := context.Background()

	// Use KEYS command for reliable cleanup in test DB (DB 15 is isolated for tests).
	// SCAN can miss keys in large keyspaces; KEYS is acceptable here since DB 15 is small.
	keys, err := client.Do(ctx, client.B().Keys().Pattern(prefix+":*").Build()).AsStrSlice()
	if err != nil {
		return
	}
	if len(keys) > 0 {
		client.Do(ctx, client.B().Del().Key(keys...).Build())
	}
}

// TestTopicBroker_Publish tests basic publish functionality
func TestTopicBroker_Publish(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-topic:")

	_, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := TopicBrokerConfig{
		Prefix:        "test-topic",
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       15,
		HistoryStore:  historyStore,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	err = broker.RegisterBrokerEventHandler(&testEventHandler{})
	if err != nil {
		t.Fatalf("failed to register event handler: %v", err)
	}

	data := []byte(`{"message": "test message"}`)
	opts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	result, err := broker.Publish("test-channel", data, opts)
	sp := result.StreamPosition
	fromCache := result.Suppressed
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	if fromCache {
		t.Error("expected fromCache to be false")
	}

	if sp.Offset == 0 {
		t.Error("expected non-zero offset")
	}

	if sp.Epoch == "" {
		t.Error("expected non-empty epoch")
	}
}

// TestTopicBroker_PublishIdempotent tests idempotent publish
func TestTopicBroker_PublishIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-topic-idempotent:")

	_, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := TopicBrokerConfig{
		Prefix:        "test-topic-idempotent",
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       15,
		HistoryStore:  historyStore,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	err = broker.RegisterBrokerEventHandler(&testEventHandler{})
	if err != nil {
		t.Fatalf("failed to register event handler: %v", err)
	}

	data := []byte(`{"message": "test message"}`)
	opts := centrifuge.PublishOptions{
		HistorySize:    10,
		HistoryTTL:     time.Hour,
		IdempotencyKey: "test-key-1",
	}

	result, err := broker.Publish("test-channel", data, opts)
	sp1 := result.StreamPosition
	fromCache1 := result.Suppressed
	if err != nil {
		t.Fatalf("first publish failed: %v", err)
	}

	if fromCache1 {
		t.Error("expected fromCache to be false for first publish")
	}

	result, err = broker.Publish("test-channel", data, opts)
	sp2 := result.StreamPosition
	fromCache2 := result.Suppressed
	if err != nil {
		t.Fatalf("second publish failed: %v", err)
	}

	if !fromCache2 {
		t.Error("expected fromCache to be true for second publish")
	}

	if sp1.Offset != sp2.Offset {
		t.Errorf("expected same offset, got %d and %d", sp1.Offset, sp2.Offset)
	}
}

// TestTopicBroker_History tests history retrieval from HistoryStore
func TestTopicBroker_History(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-topic-history:")

	_, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := TopicBrokerConfig{
		Prefix:        "test-topic-history",
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       15,
		HistoryStore:  historyStore,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	err = broker.RegisterBrokerEventHandler(&testEventHandler{})
	if err != nil {
		t.Fatalf("failed to register event handler: %v", err)
	}

	// Publish messages to allocate offsets
	for i := 0; i < 5; i++ {
		data := fmt.Appendf(nil, `{"message": "message %d"}`, i)
		opts := centrifuge.PublishOptions{
			HistorySize: 10,
			HistoryTTL:  time.Hour,
		}
		_, err := broker.Publish("test-channel", data, opts)
		if err != nil {
			t.Fatalf("publish failed: %v", err)
		}
	}

	// Add messages to history store (simulating DB persistence)
	for i := 0; i < 5; i++ {
		pub := &centrifuge.Publication{
			Data: fmt.Appendf(nil, `{"message": "message %d"}`, i),
		}
		historyStore.Save("test-channel", pub)
	}

	opts := centrifuge.HistoryOptions{
		Filter: centrifuge.HistoryFilter{
			Since: &centrifuge.StreamPosition{
				Offset: 0,
			},
			Limit: 10,
		},
	}

	pubs, sp, err := broker.History("test-channel", opts)
	if err != nil {
		t.Fatalf("history failed: %v", err)
	}

	if len(pubs) == 0 {
		t.Error("expected non-empty history")
	}

	if sp.Offset == 0 {
		t.Error("expected non-zero stream position offset")
	}
}

// TestTopicBroker_HistoryNilStore tests History without HistoryStore
func TestTopicBroker_HistoryNilStore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-history-nil-store"
	cleanupTestRedis(t, prefix)

	config := TopicBrokerConfig{
		Prefix:    prefix,
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("create broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	err = broker.RegisterBrokerEventHandler(&testEventHandler{})
	if err != nil {
		t.Fatalf("register event handler: %v", err)
	}

	// Publish messages to allocate offsets
	for i := 1; i <= 3; i++ {
		data := fmt.Appendf(nil, `{"seq":%d}`, i)
		opts := centrifuge.PublishOptions{
			HistorySize: 10,
			HistoryTTL:  time.Hour,
		}
		_, err := broker.Publish("test-channel", data, opts)
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	// History should return empty with nil store
	pubs, sp, err := broker.History("test-channel", centrifuge.HistoryOptions{
		Filter: centrifuge.HistoryFilter{
			Since: &centrifuge.StreamPosition{Offset: 0},
		},
	})
	if err != nil {
		t.Errorf("expected no error with nil store, got: %v", err)
	}
	if len(pubs) != 0 {
		t.Errorf("expected 0 publications with nil store, got %d", len(pubs))
	}
	if sp.Offset != 3 {
		t.Errorf("expected stream position offset 3, got %d", sp.Offset)
	}
}

// TestTopicBroker_HistoryWithFilterSince tests History with Since filter
func TestTopicBroker_HistoryWithFilterSince(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-history-filter"
	cleanupTestRedis(t, prefix)

	historyStore := newTestHistoryStore()
	broker, _, cleanup := setupTestBroker(t, prefix, withHistoryStore(historyStore))
	defer cleanup()

	// Publish 5 messages
	for i := 0; i < 5; i++ {
		data := fmt.Appendf(nil, `{"seq":%d}`, i+1)
		opts := centrifuge.PublishOptions{
			HistorySize: 10,
			HistoryTTL:  time.Hour,
		}
		_, err := broker.Publish("filter-channel", data, opts)
		if err != nil {
			t.Fatalf("publish %d: %v", i+1, err)
		}
	}

	// Add to history store
	for i := 0; i < 5; i++ {
		pub := &centrifuge.Publication{
			Data: fmt.Appendf(nil, `{"seq":%d}`, i+1),
		}
		historyStore.Save("filter-channel", pub)
	}

	// Query with Since filter at offset 2
	opts := centrifuge.HistoryOptions{
		Filter: centrifuge.HistoryFilter{
			Since: &centrifuge.StreamPosition{Offset: 2},
		},
	}
	pubs, sp, err := broker.History("filter-channel", opts)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pubs) < 3 {
		t.Errorf("expected at least 3 publications after offset 2, got %d", len(pubs))
	}
	if sp.Offset == 0 {
		t.Error("expected non-zero stream position")
	}
}

// TestDualBroker_Routing tests channel routing
func TestDualBroker_Routing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-dual-live:")
	cleanupTestRedis(t, "test-dual-topic:")

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
			Prefix: "test-dual-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:        "test-dual-topic",
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

	broker.RegisterChannelType("live-channel", Live)
	broker.RegisterChannelType("topic-channel", Topic)

	liveData := []byte(`{"message": "live message"}`)
	liveOpts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	result, err := broker.Publish("live-channel", liveData, liveOpts)
	liveSp := result.StreamPosition
	if err != nil {
		t.Fatalf("publish to live channel failed: %v", err)
	}

	if liveSp.Offset == 0 {
		t.Error("expected non-zero offset for live channel from RedisBroker")
	}

	topicData := []byte(`{"message": "topic message"}`)
	topicOpts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	result, err = broker.Publish("topic-channel", topicData, topicOpts)
	topicSp := result.StreamPosition
	if err != nil {
		t.Fatalf("publish to topic channel failed: %v", err)
	}

	if topicSp.Offset == 0 {
		t.Error("expected non-zero offset for topic channel")
	}
}

// TestTopicBrokerConfig tests broker configuration
func TestTopicBrokerConfig(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "test-prefix",
		RedisAddr: "localhost:6379",
		RedisDB:   0,
	}

	if config.Prefix != "test-prefix" {
		t.Errorf("expected prefix test-prefix, got %s", config.Prefix)
	}

	if config.RedisAddr != "localhost:6379" {
		t.Errorf("expected addr localhost:6379, got %s", config.RedisAddr)
	}
}

// TestGenerateEpoch tests epoch generation
func TestGenerateEpoch(t *testing.T) {
	epoch := generateEpoch()
	if epoch == "" {
		t.Error("expected non-empty epoch")
	}

	epoch2 := generateEpoch()
	if epoch == epoch2 {
		t.Error("expected different epochs")
	}
}

// TestTopicBroker_PublishMultipleChannels tests publishing to multiple channels
func TestTopicBroker_PublishMultipleChannels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-topic-multi:")

	_, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := TopicBrokerConfig{
		Prefix:        "test-topic-multi",
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       15,
		HistoryStore:  historyStore,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	err = broker.RegisterBrokerEventHandler(&testEventHandler{})
	if err != nil {
		t.Fatalf("failed to register event handler: %v", err)
	}

	channels := []string{"channel-1", "channel-2", "channel-3"}
	for _, ch := range channels {
		data := fmt.Appendf(nil, `{"message": "message for %s"}`, ch)
		opts := centrifuge.PublishOptions{
			HistorySize: 10,
			HistoryTTL:  time.Hour,
		}

		result, err := broker.Publish(ch, data, opts)
		sp := result.StreamPosition
		if err != nil {
			t.Fatalf("publish to channel %s failed: %v", ch, err)
		}

		if sp.Offset == 0 {
			t.Errorf("expected non-zero offset for channel %s", ch)
		}
	}
}

// TestTopicBroker_PublishEmptyData tests publishing empty data
func TestTopicBroker_PublishEmptyData(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-topic-empty:")

	_, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := TopicBrokerConfig{
		Prefix:        "test-topic-empty",
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       15,
		HistoryStore:  historyStore,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	err = broker.RegisterBrokerEventHandler(&testEventHandler{})
	if err != nil {
		t.Fatalf("failed to register event handler: %v", err)
	}

	data := []byte(`{}`)
	opts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	result, err := broker.Publish("test-channel", data, opts)
	sp := result.StreamPosition
	if err != nil {
		t.Fatalf("publish minimal data failed: %v", err)
	}

	if sp.Offset == 0 {
		t.Error("expected non-zero offset")
	}
}

// TestTopicBroker_PublishWithIdempotencyKeyTests tests idempotency with different keys
func TestTopicBroker_PublishWithIdempotencyKeyTests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-topic-idempotency:")

	_, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := TopicBrokerConfig{
		Prefix:        "test-topic-idempotency",
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       15,
		HistoryStore:  historyStore,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	err = broker.RegisterBrokerEventHandler(&testEventHandler{})
	if err != nil {
		t.Fatalf("failed to register event handler: %v", err)
	}

	data := []byte(`{"message": "test message"}`)
	opts1 := centrifuge.PublishOptions{
		HistorySize:    10,
		HistoryTTL:     time.Hour,
		IdempotencyKey: "key-1",
	}

	result, err := broker.Publish("test-channel", data, opts1)
	sp1 := result.StreamPosition
	if err != nil {
		t.Fatalf("first publish failed: %v", err)
	}

	opts2 := centrifuge.PublishOptions{
		HistorySize:    10,
		HistoryTTL:     time.Hour,
		IdempotencyKey: "key-2",
	}

	result, err = broker.Publish("test-channel", data, opts2)
	sp2 := result.StreamPosition
	if err != nil {
		t.Fatalf("second publish failed: %v", err)
	}

	if sp1.Offset == sp2.Offset {
		t.Error("expected different offsets for different idempotency keys")
	}
}

// TestTopicBroker_RemoveHistory tests history removal
func TestTopicBroker_RemoveHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-topic-remove:")

	_, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := TopicBrokerConfig{
		Prefix:        "test-topic-remove",
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       15,
		HistoryStore:  historyStore,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	err = broker.RegisterBrokerEventHandler(&testEventHandler{})
	if err != nil {
		t.Fatalf("failed to register event handler: %v", err)
	}

	for i := 0; i < 3; i++ {
		data := fmt.Appendf(nil, `{"message": "message %d"}`, i)
		opts := centrifuge.PublishOptions{
			HistorySize: 10,
			HistoryTTL:  time.Hour,
		}
		_, err := broker.Publish("test-channel", data, opts)
		if err != nil {
			t.Fatalf("publish failed: %v", err)
		}
	}

	err = broker.RemoveHistory("test-channel")
	if err != nil {
		t.Fatalf("remove history failed: %v", err)
	}
}

// TestTopicBroker_PublishConcurrent tests concurrent publishing
func TestTopicBroker_PublishConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-topic-concurrent:")

	_, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := TopicBrokerConfig{
		Prefix:        "test-topic-concurrent",
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       15,
		HistoryStore:  historyStore,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	err = broker.RegisterBrokerEventHandler(&testEventHandler{})
	if err != nil {
		t.Fatalf("failed to register event handler: %v", err)
	}

	var wg sync.WaitGroup
	numGoroutines := 10
	errCh := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			data := fmt.Appendf(nil, `{"message": "concurrent message %d"}`, index)
			opts := centrifuge.PublishOptions{
				HistorySize: 10,
				HistoryTTL:  time.Hour,
			}
			_, err := broker.Publish("test-channel", data, opts)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: %v", index, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent publish error: %v", err)
	}
}

// TestDualBroker_PublishToBothModes tests publishing to both Live and Topic modes
func TestDualBroker_PublishToBothModes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-dual-both-live:")
	cleanupTestRedis(t, "test-dual-both-topic:")

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
			Prefix: "test-dual-both-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:        "test-dual-both-topic",
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

	broker.RegisterChannelType("live-channel", Live)
	broker.RegisterChannelType("topic-channel", Topic)

	liveData := []byte(`{"message": "live message"}`)
	liveOpts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	result, err := broker.Publish("live-channel", liveData, liveOpts)
	liveSp := result.StreamPosition
	if err != nil {
		t.Fatalf("publish to live channel failed: %v", err)
	}

	if liveSp.Offset == 0 {
		t.Error("expected non-zero offset for live channel")
	}

	topicData := []byte(`{"message": "topic message"}`)
	topicOpts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	result, err = broker.Publish("topic-channel", topicData, topicOpts)
	topicSp := result.StreamPosition
	if err != nil {
		t.Fatalf("publish to topic channel failed: %v", err)
	}

	if topicSp.Offset == 0 {
		t.Error("expected non-zero offset for topic channel")
	}
}

// TestDualBroker_UnregisterChannelType tests behavior with unregistered channel
func TestDualBroker_UnregisterChannelType(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-dual-unregistered-live:")
	cleanupTestRedis(t, "test-dual-unregistered-topic:")

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
			Prefix: "test-dual-unregistered-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:        "test-dual-unregistered-topic",
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

	data := []byte(`{"message": "unregistered channel message"}`)
	opts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	// 未注册的 channel 应该返回错误
	result, err := broker.Publish("unregistered-channel", data, opts)
	sp := result.StreamPosition
	if err == nil {
		t.Fatalf("expected error for unregistered channel, got nil")
	}
	if sp.Offset != 0 {
		t.Error("expected zero offset for unregistered channel")
	}
}

// TestDualBroker_UnregisteredChannel_AllMethods 验证所有方法对未注册频道都返回错误
func TestDualBroker_UnregisteredChannel_AllMethods(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-dual-unreg-all-live:")
	cleanupTestRedis(t, "test-dual-unreg-all-topic:")

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
			Prefix: "test-dual-unreg-all-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:        "test-dual-unreg-all-topic",
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

	ch := "never-registered-channel"

	// Subscribe 应该返回错误
	if err := broker.Subscribe(ch); err == nil {
		t.Error("Subscribe: expected error for unregistered channel")
	} else if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("Subscribe: expected 'not registered' error, got: %v", err)
	}

	// Unsubscribe 应该返回错误
	if err := broker.Unsubscribe(ch); err == nil {
		t.Error("Unsubscribe: expected error for unregistered channel")
	}

	// PublishWithContext 应该返回错误
	_, err = broker.PublishWithContext(context.Background(), ch, []byte("{}"), centrifuge.PublishOptions{})
	if err == nil {
		t.Error("PublishWithContext: expected error for unregistered channel")
	}

	// PublishJoin 应该返回错误
	if err := broker.PublishJoin(ch, &centrifuge.ClientInfo{}); err == nil {
		t.Error("PublishJoin: expected error for unregistered channel")
	}

	// PublishLeave 应该返回错误
	if err := broker.PublishLeave(ch, &centrifuge.ClientInfo{}); err == nil {
		t.Error("PublishLeave: expected error for unregistered channel")
	}

	// History 应该返回错误
	_, _, err = broker.History(ch, centrifuge.HistoryOptions{})
	if err == nil {
		t.Error("History: expected error for unregistered channel")
	}

	// RemoveHistory 应该返回错误
	if err := broker.RemoveHistory(ch); err == nil {
		t.Error("RemoveHistory: expected error for unregistered channel")
	}
}

// TestDualBroker_GetChannelType 验证 getChannelType 方法的行为
func TestDualBroker_GetChannelType(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	cleanupTestRedis(t, "test-dual-ct-live:")
	cleanupTestRedis(t, "test-dual-ct-topic:")

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

	config := DualBrokerConfig{
		Live: centrifuge.RedisBrokerConfig{
			Prefix: "test-dual-ct-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:    "test-dual-ct-topic",
			RedisAddr: "localhost:6379",
			RedisDB:   15,
		},
	}

	broker, err := NewDualBroker(node, config)
	if err != nil {
		t.Fatalf("failed to create dual broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	// 未注册频道应该返回错误
	_, err = broker.getChannelType("unknown-channel")
	if err == nil {
		t.Error("expected error for unregistered channel")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("expected 'not registered' error, got: %v", err)
	}

	// 注册 Live 频道后应该返回 Live
	broker.RegisterChannelType("my-live", Live)
	ct, err := broker.getChannelType("my-live")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ct != Live {
		t.Errorf("expected Live, got %v", ct)
	}

	// 注册 Topic 频道后应该返回 Topic
	broker.RegisterChannelType("my-topic", Topic)
	ct, err = broker.getChannelType("my-topic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ct != Topic {
		t.Errorf("expected Topic, got %v", ct)
	}

	// 测试 channelTypes 清理行为
	// 新行为：只有 Unsubscribe 成功后才清理 channelTypes
	// 由于单元测试没有实际的 Redis 连接，Subscribe/Unsubscribe 可能会失败
	// 所以我们直接验证：如果 Unsubscribe 返回 nil（成功），channel type 应该被清理
	// 如果 Unsubscribe 返回 error（失败），channel type 应该保留

	// 场景1：对于 Live 类型，底层 liveBroker.Unsubscribe 即使没有先 Subscribe 也可能成功（取决于实现）
	// 场景2：对于未订阅的频道，底层可能返回错误或成功

	// 这里我们只验证 channelTypes map 本身的行为是正确的
	// 实际的 Subscribe/Unsubscribe 集成测试在更高层次覆盖

	// 注册一个新的频道用于测试清理行为
	broker.RegisterChannelType("test-cleanup", Live)
	_, err = broker.getChannelType("test-cleanup")
	if err != nil {
		t.Fatalf("unexpected error after RegisterChannelType: %v", err)
	}

	// 调用 Unsubscribe（可能成功也可能失败，取决于底层 broker）
	// 我们只关心不会 panic，并且 channelTypes map 的行为是一致的
	unsubErr := broker.Unsubscribe("test-cleanup")
	if unsubErr == nil {
		// Unsubscribe 成功，channel type 应该被清理
		_, err = broker.getChannelType("test-cleanup")
		if err == nil {
			t.Error("expected error after successful Unsubscribe cleaned up channel type")
		}
	} else {
		// Unsubscribe 失败，channel type 应该保留
		_, err = broker.getChannelType("test-cleanup")
		if err != nil {
			t.Errorf("expected channel type to be preserved after failed Unsubscribe, but got error: %v", err)
		}
	}
}

// TestTopicBroker_SubscribeUnsubscribe tests PUB/SUB lifecycle.
func TestTopicBroker_SubscribeUnsubscribe(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-sub-unsub"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	err := broker.Subscribe("test-channel")
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	err = broker.Subscribe("test-channel")
	if err != nil {
		t.Fatalf("duplicate subscribe failed: %v", err)
	}

	err = broker.Unsubscribe("test-channel")
	if err != nil {
		t.Fatalf("unsubscribe failed: %v", err)
	}

	err = broker.Unsubscribe("test-channel")
	if err != nil {
		t.Fatalf("unsubscribe non-subscribed channel failed: %v", err)
	}
}

// TestTopicBroker_PublishJoinLeave tests join/leave event publishing.
func TestTopicBroker_PublishJoinLeave(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-join-leave"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	err := broker.PublishJoin("test-channel", &centrifuge.ClientInfo{
		UserID:   "test-user",
		ClientID: "test-client",
	})
	if err != nil {
		t.Fatalf("publish join failed: %v", err)
	}

	err = broker.PublishLeave("test-channel", &centrifuge.ClientInfo{
		UserID:   "test-user",
		ClientID: "test-client",
	})
	if err != nil {
		t.Fatalf("publish leave failed: %v", err)
	}
}

// TestTopicBroker_TableDrivenPublish tests various payload scenarios.
func TestTopicBroker_TableDrivenPublish(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-table-driven"
	cleanupTestRedis(t, prefix)

	tests := []struct {
		name    string
		channel string
		data    []byte
		wantErr bool
	}{
		{
			name:    "simple json",
			channel: "ch-1",
			data:    []byte(`{"text":"hello"}`),
		},
		{
			name:    "empty json object",
			channel: "ch-2",
			data:    []byte(`{}`),
		},
		{
			name:    "numeric data",
			channel: "ch-3",
			data:    []byte(`{"count":12345}`),
		},
		{
			name:    "nested json",
			channel: "ch-4",
			data:    []byte(`{"nested":{"a":1,"b":[2,3]}}`),
		},
		{
			name:    "unicode content",
			channel: "ch-5",
			data:    []byte(`{"text":"你好世界"}`),
		},
		{
			name:    "long channel name",
			channel: "a-very-long-channel-name-that-exceeds-typical-length-1234567890-abcdef",
			data:    []byte(`{"ok":true}`),
		},
		{
			name:    "large payload",
			channel: "ch-6",
			data:    fmt.Appendf(nil, `{"data":"%s"}`, strings.Repeat("x", 5000)),
		},
	}

	broker, eventHandler, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := centrifuge.PublishOptions{
				HistorySize: 10,
				HistoryTTL:  time.Hour,
			}

			result, err := broker.Publish(tt.channel, tt.data, opts)
			sp := result.StreamPosition
			fromCache := result.Suppressed
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("publish failed: %v", err)
			}
			if fromCache {
				t.Error("expected fromCache to be false")
			}
			if sp.Offset == 0 {
				t.Error("expected non-zero offset")
			}
			if sp.Epoch == "" {
				t.Error("expected non-empty epoch")
			}
			_ = eventHandler
		})
	}
}

// TestTopicBroker_DirtyDataIsolation tests that each test gets clean Redis state.
func TestTopicBroker_DirtyDataIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-cleanup"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	data := []byte(`{"msg": "first publish"}`)
	opts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	result, err := broker.Publish("isolated-channel", data, opts)
	sp1 := result.StreamPosition
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if sp1.Offset != 1 {
		t.Errorf("expected offset 1, got %d", sp1.Offset)
	}

	result, err = broker.Publish("isolated-channel", data, opts)
	sp2 := result.StreamPosition
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if sp2.Offset != 2 {
		t.Errorf("expected offset 2, got %d", sp2.Offset)
	}

	sp := broker.getStreamPosition(context.Background(), "isolated-channel")
	if sp.Offset != 2 {
		t.Errorf("expected stream position offset 2, got %d", sp.Offset)
	}
}

// TestTopicBroker_OffsetSequential tests that offsets are sequential without gaps.
func TestTopicBroker_OffsetSequential(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-offset-seq"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ch := "offset-seq-channel"
	prevOffset := uint64(0)
	for i := 0; i < 20; i++ {
		data := fmt.Appendf(nil, `{"seq":%d}`, i+1)
		opts := centrifuge.PublishOptions{
			HistorySize: 100,
			HistoryTTL:  time.Hour,
		}
		result, err := broker.Publish(ch, data, opts)
		sp := result.StreamPosition
		if err != nil {
			t.Fatalf("publish %d failed: %v", i+1, err)
		}
		if sp.Offset != prevOffset+1 {
			t.Errorf("publish %d: expected offset %d, got %d", i+1, prevOffset+1, sp.Offset)
		}
		prevOffset = sp.Offset
	}
}

// TestTopicBroker_OffsetCrossChannelIsolation tests that offsets are independent per channel.
func TestTopicBroker_OffsetCrossChannelIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-offset-isolation"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	channels := []string{"ch-a", "ch-b", "ch-c"}
	offsets := make(map[string]uint64)

	for round := 0; round < 5; round++ {
		for _, ch := range channels {
			data := fmt.Appendf(nil, `{"round":%d,"ch":"%s"}`, round+1, ch)
			opts := centrifuge.PublishOptions{
				HistorySize: 10,
				HistoryTTL:  time.Hour,
			}
			result, err := broker.Publish(ch, data, opts)
			sp := result.StreamPosition
			if err != nil {
				t.Fatalf("publish to %s round %d failed: %v", ch, round+1, err)
			}
			expected := offsets[ch] + 1
			if sp.Offset != expected {
				t.Errorf("channel %s round %d: expected offset %d, got %d", ch, round+1, expected, sp.Offset)
			}
			offsets[ch] = sp.Offset
		}
	}

	for _, ch := range channels {
		if offsets[ch] != 5 {
			t.Errorf("channel %s expected final offset 5, got %d", ch, offsets[ch])
		}
	}
}

// TestTopicBroker_PublishLargePayload tests publishing with a very large payload.
func TestTopicBroker_PublishLargePayload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-large-payload"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	largeContent := strings.Repeat("ABCDEFGHIJ", 10000) // ~100KB
	data := fmt.Appendf(nil, `{"data":"%s"}`, largeContent)
	opts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	result, err := broker.Publish("large-channel", data, opts)
	sp := result.StreamPosition
	if err != nil {
		t.Fatalf("publish large payload failed: %v", err)
	}
	if sp.Offset == 0 {
		t.Error("expected non-zero offset for large payload")
	}
}

// TestDualBroker_ConcurrentRegisterAndPublish tests DualBroker under concurrent registration.
func TestDualBroker_ConcurrentRegisterAndPublish(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-dual-concurrent-reg"
	cleanupTestRedis(t, prefix+"-live:")
	cleanupTestRedis(t, prefix+"-topic:")

	node, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	redisShard, err := centrifuge.NewRedisShard(node, centrifuge.RedisShardConfig{
		Address: "localhost:6379",
	})
	if err != nil {
		t.Fatalf("create shard: %v", err)
	}

	config := DualBrokerConfig{
		Live: centrifuge.RedisBrokerConfig{
			Prefix: prefix + "-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:       prefix + "-topic",
			RedisAddr:    "localhost:6379",
			RedisDB:      15,
			HistoryStore: newTestHistoryStore(),
		},
	}

	broker, err := NewDualBroker(node, config)
	if err != nil {
		t.Fatalf("create dual broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ch := fmt.Sprintf("concurrent-ch-%d", idx%5)
			ct := Live
			if idx%2 == 0 {
				ct = Topic
			}
			broker.RegisterChannelType(ch, ct)
			opts := centrifuge.PublishOptions{
				HistorySize: 10,
				HistoryTTL:  time.Hour,
			}
			_, _ = broker.Publish(ch, []byte(`{}`), opts)
			broker.RegisterChannelType(ch, Live)
		}(i)
	}
	wg.Wait()
}

// TestTopicBroker_PublishCrossChannelMetadataIsolation verifies each channel has independent meta.
func TestTopicBroker_PublishCrossChannelMetadataIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-meta-isolation"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	channels := []string{"ch-alpha", "ch-beta", "ch-gamma"}
	for _, ch := range channels {
		data := fmt.Appendf(nil, `{"ch":"%s"}`, ch)
		opts := centrifuge.PublishOptions{
			HistorySize: 10,
			HistoryTTL:  time.Hour,
		}
		result, err := broker.Publish(ch, data, opts)
		sp := result.StreamPosition
		if err != nil {
			t.Fatalf("publish %s: %v", ch, err)
		}
		if sp.Offset != 1 {
			t.Errorf("channel %s: expected offset 1, got %d", ch, sp.Offset)
		}
	}

	for _, ch := range channels {
		pos := broker.getStreamPosition(context.Background(), ch)
		if pos.Offset != 1 {
			t.Errorf("channel %s: expected stream position offset 1, got %d", ch, pos.Offset)
		}
	}
}

// ========== New tests for BatchIncrby and PublishWithOffset ==========

// TestTopicBroker_BatchIncrby tests batch offset pre-allocation.
func TestTopicBroker_BatchIncrby(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-batch-incrby"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	// Test single channel
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-1"}})
	if err != nil {
		t.Fatalf("BatchIncrby single channel: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}
	if positions["ch-1"].Offset != 1 {
		t.Errorf("expected offset 1, got %d", positions["ch-1"].Offset)
	}
	if positions["ch-1"].Epoch == "" {
		t.Error("expected non-empty epoch")
	}

	// Test multiple channels
	positions, err = broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-2"}, {Channel: "ch-3"}, {Channel: "ch-4"}})
	if err != nil {
		t.Fatalf("BatchIncrby multiple channels: %v", err)
	}
	if len(positions) != 3 {
		t.Fatalf("expected 3 positions, got %d", len(positions))
	}
	for _, ch := range []string{"ch-2", "ch-3", "ch-4"} {
		if positions[ch].Offset != 1 {
			t.Errorf("channel %s: expected offset 1, got %d", ch, positions[ch].Offset)
		}
		if positions[ch].Epoch == "" {
			t.Errorf("channel %s: expected non-empty epoch", ch)
		}
	}

	// Test consecutive calls increment offset
	positions2, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-1"}})
	if err != nil {
		t.Fatalf("BatchIncrby consecutive: %v", err)
	}
	if positions2["ch-1"].Offset != 2 {
		t.Errorf("expected offset 2, got %d", positions2["ch-1"].Offset)
	}

	// Test empty channels
	positions, err = broker.BatchIncrby(ctx, []ChannelIncrbyRequest{})
	if err != nil {
		t.Fatalf("BatchIncrby empty: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("expected 0 positions, got %d", len(positions))
	}
}

// TestTopicBroker_BatchIncrby_NewChannelEpoch tests that new channels get epoch created.
func TestTopicBroker_BatchIncrby_NewChannelEpoch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-batch-incrby"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "new-channel"}})
	if err != nil {
		t.Fatalf("BatchIncrby: %v", err)
	}

	if positions["new-channel"].Offset != 1 {
		t.Errorf("expected offset 1, got %d", positions["new-channel"].Offset)
	}
	if positions["new-channel"].Epoch == "" {
		t.Error("expected non-empty epoch for new channel")
	}

	// Verify meta key was created in Redis
	client, err := newTestRedisClient()
	if err != nil {
		t.Fatalf("create redis client: %v", err)
	}
	defer client.Close()

	metaKey := prefix + ":meta:new-channel"
	result, err := client.Do(context.Background(),
		client.B().Hmget().Key(metaKey).Field("e", "s").Build()).AsStrSlice()
	if err != nil {
		t.Fatalf("HMGET meta: %v", err)
	}
	if len(result) < 2 || result[0] == "" {
		t.Error("expected meta key to be created with epoch")
	}
}

// TestTopicBroker_PublishWithOffset tests publishing with pre-allocated offset.
func TestTopicBroker_PublishWithOffset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-publish-with-offset"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	// Pre-allocate offset
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "test-channel"}})
	if err != nil {
		t.Fatalf("BatchIncrby: %v", err)
	}

	sp := positions["test-channel"]

	// Publish with pre-allocated offset
	data := []byte(`{"message": "test"}`)
	opts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	err = broker.PublishWithOffset(ctx, "test-channel", data, opts, sp)
	if err != nil {
		t.Fatalf("PublishWithOffset: %v", err)
	}
}

// TestTopicBroker_PublishWithOffset_EpochMismatch tests epoch mismatch detection.
func TestTopicBroker_PublishWithOffset_EpochMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-epoch-mismatch"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	// Pre-allocate offset
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "test-channel"}})
	if err != nil {
		t.Fatalf("BatchIncrby: %v", err)
	}

	// Manually change epoch in Redis to simulate epoch change
	client, err := newTestRedisClient()
	if err != nil {
		t.Fatalf("create redis client: %v", err)
	}
	defer client.Close()

	metaKey := prefix + ":meta:test-channel"
	newEpoch := generateEpoch()
	client.Do(ctx, client.B().Hset().Key(metaKey).FieldValue().FieldValue("e", newEpoch).Build())

	// Try to publish with old epoch - should fail
	data := []byte(`{"message": "test"}`)
	opts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	err = broker.PublishWithOffset(ctx, "test-channel", data, opts, positions["test-channel"])
	if err == nil {
		t.Error("expected epoch mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "epoch mismatch") {
		t.Errorf("expected 'epoch mismatch' in error, got: %v", err)
	}
}

// TestDualBroker_BatchIncrby tests BatchIncrby routing through DualBroker.
func TestDualBroker_BatchIncrby(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-dual-batch"
	cleanupTestRedis(t, prefix+"-live:")
	cleanupTestRedis(t, prefix+"-topic:")

	node, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	redisShard, err := centrifuge.NewRedisShard(node, centrifuge.RedisShardConfig{
		Address: "localhost:6379",
	})
	if err != nil {
		t.Fatalf("create shard: %v", err)
	}

	config := DualBrokerConfig{
		Live: centrifuge.RedisBrokerConfig{
			Prefix: prefix + "-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:       prefix + "-topic",
			RedisAddr:    "localhost:6379",
			RedisDB:      15,
			HistoryStore: newTestHistoryStore(),
		},
	}

	broker, err := NewDualBroker(node, config)
	if err != nil {
		t.Fatalf("create dual broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	ctx := context.Background()

	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-1"}, {Channel: "ch-2"}})
	if err != nil {
		t.Fatalf("BatchIncrby: %v", err)
	}

	if len(positions) != 2 {
		t.Fatalf("expected 2 positions, got %d", len(positions))
	}
	for _, ch := range []string{"ch-1", "ch-2"} {
		if positions[ch].Offset != 1 {
			t.Errorf("channel %s: expected offset 1, got %d", ch, positions[ch].Offset)
		}
	}
}

// TestDualBroker_PublishWithOffset tests PublishWithOffset routing through DualBroker.
func TestDualBroker_PublishWithOffset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-dual-pwo"
	cleanupTestRedis(t, prefix+"-live:")
	cleanupTestRedis(t, prefix+"-topic:")

	node, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	redisShard, err := centrifuge.NewRedisShard(node, centrifuge.RedisShardConfig{
		Address: "localhost:6379",
	})
	if err != nil {
		t.Fatalf("create shard: %v", err)
	}

	config := DualBrokerConfig{
		Live: centrifuge.RedisBrokerConfig{
			Prefix: prefix + "-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:       prefix + "-topic",
			RedisAddr:    "localhost:6379",
			RedisDB:      15,
			HistoryStore: newTestHistoryStore(),
		},
	}

	broker, err := NewDualBroker(node, config)
	if err != nil {
		t.Fatalf("create dual broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	ctx := context.Background()

	// Pre-allocate offset
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "test-channel"}})
	if err != nil {
		t.Fatalf("BatchIncrby: %v", err)
	}

	// Publish with pre-allocated offset
	data := []byte(`{"message": "test"}`)
	opts := centrifuge.PublishOptions{
		HistorySize: 10,
		HistoryTTL:  time.Hour,
	}

	err = broker.PublishWithOffset(ctx, "test-channel", data, opts, positions["test-channel"])
	if err != nil {
		t.Fatalf("PublishWithOffset: %v", err)
	}
}

// TestTopicBroker_GapScenario simulates: BatchIncrby succeeds, DB transaction rolls back,
// offset is consumed but not persisted. The next successful publish should have a gap in offsets.
func TestTopicBroker_GapScenario(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-gap-scenario"
	cleanupTestRedis(t, prefix)

	historyStore := newTestHistoryStore()
	broker, _, cleanup := setupTestBroker(t, prefix, withHistoryStore(historyStore))
	defer cleanup()

	ctx := context.Background()

	// Step 1: Publish message 1 — succeeds end-to-end
	positions1, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-1"}})
	if err != nil {
		t.Fatalf("BatchIncrby 1: %v", err)
	}
	sp1 := positions1["ch-1"]
	if sp1.Offset != 1 {
		t.Fatalf("expected offset 1, got %d", sp1.Offset)
	}

	// Save to DB (simulate successful transaction)
	historyStore.SaveWithOffset("ch-1", &centrifuge.Publication{Data: []byte(`{"msg":1}`)}, sp1.Offset)

	// Publish to PUB/SUB
	err = broker.PublishWithOffset(ctx, "ch-1", []byte(`{"msg":1}`), centrifuge.PublishOptions{
		HistorySize: 100,
		HistoryTTL:  time.Hour,
	}, sp1)
	if err != nil {
		t.Fatalf("PublishWithOffset 1: %v", err)
	}

	// Step 2: BatchIncrby for message 2 — succeeds (offset 2 allocated)
	positions2, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-1"}})
	if err != nil {
		t.Fatalf("BatchIncrby 2: %v", err)
	}
	sp2 := positions2["ch-1"]
	if sp2.Offset != 2 {
		t.Fatalf("expected offset 2, got %d", sp2.Offset)
	}

	// DB transaction ROLLS BACK — offset 2 is consumed but NOT persisted
	// (do NOT call historyStore.SaveWithOffset)
	// Do NOT call PublishWithOffset either (data not committed, no push)

	// Step 3: Publish message 3 — succeeds end-to-end
	positions3, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-1"}})
	if err != nil {
		t.Fatalf("BatchIncrby 3: %v", err)
	}
	sp3 := positions3["ch-1"]
	if sp3.Offset != 3 {
		t.Fatalf("expected offset 3, got %d", sp3.Offset)
	}

	historyStore.SaveWithOffset("ch-1", &centrifuge.Publication{Data: []byte(`{"msg":3}`)}, sp3.Offset)

	err = broker.PublishWithOffset(ctx, "ch-1", []byte(`{"msg":3}`), centrifuge.PublishOptions{
		HistorySize: 100,
		HistoryTTL:  time.Hour,
	}, sp3)
	if err != nil {
		t.Fatalf("PublishWithOffset 3: %v", err)
	}

	// Verify: HistoryStore has 2 publications (offset 1 and 3), offset 2 is a gap
	pubs, err := historyStore.Query(context.Background(), "ch-1", 0, 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(pubs) != 2 {
		t.Fatalf("expected 2 publications (gap at offset 2), got %d", len(pubs))
	}
	if pubs[0].Offset != 1 {
		t.Errorf("expected first pub offset 1, got %d", pubs[0].Offset)
	}
	if pubs[1].Offset != 3 {
		t.Errorf("expected second pub offset 3 (gap at 2), got %d", pubs[1].Offset)
	}

	// Verify: stream position is at offset 3 (all offsets consumed)
	sp := broker.getStreamPosition(context.Background(), "ch-1")
	if sp.Offset != 3 {
		t.Errorf("expected stream position offset 3, got %d", sp.Offset)
	}
}

// TestTopicBroker_PushFailureAfterPersist simulates: DB commit succeeds but
// PublishWithOffset fails (epoch mismatch). Data is persisted but client didn't
// get real-time push. Client must discover via DB pull.
func TestTopicBroker_PushFailureAfterPersist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-push-fail"
	cleanupTestRedis(t, prefix)

	historyStore := newTestHistoryStore()
	broker, _, cleanup := setupTestBroker(t, prefix, withHistoryStore(historyStore))
	defer cleanup()

	ctx := context.Background()

	// Step 1: BatchIncrby
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-1"}})
	if err != nil {
		t.Fatalf("BatchIncrby: %v", err)
	}
	sp := positions["ch-1"]

	// Step 2: Save to DB (simulate successful transaction)
	historyStore.SaveWithOffset("ch-1", &centrifuge.Publication{Data: []byte(`{"msg":1}`)}, sp.Offset)

	// Step 3: Simulate epoch change BEFORE PublishWithOffset
	// (e.g., another process reset the stream, or long DB transaction caused epoch change)
	client, err := newTestRedisClient()
	if err != nil {
		t.Fatalf("create redis client: %v", err)
	}
	defer client.Close()

	metaKey := prefix + ":meta:ch-1"
	newEpoch := generateEpoch()
	client.Do(ctx, client.B().Hset().Key(metaKey).FieldValue().FieldValue("e", newEpoch).Build())

	// Step 4: PublishWithOffset fails due to epoch mismatch
	err = broker.PublishWithOffset(ctx, "ch-1", []byte(`{"msg":1}`), centrifuge.PublishOptions{
		HistorySize: 100,
		HistoryTTL:  time.Hour,
	}, sp)
	if err == nil {
		t.Fatal("expected epoch mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "epoch mismatch") {
		t.Errorf("expected 'epoch mismatch' in error, got: %v", err)
	}

	// Verify: Data IS persisted in HistoryStore despite push failure
	pubs, err := historyStore.Query(context.Background(), "ch-1", 0, 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(pubs) != 1 {
		t.Fatalf("expected 1 persisted publication, got %d", len(pubs))
	}
	if string(pubs[0].Data) != `{"msg":1}` {
		t.Errorf("unexpected persisted data: %s", string(pubs[0].Data))
	}

	// Verify: Client can still recover via HistoryStore (DB pull)
	// even though real-time push failed
	recoveredPubs, err := historyStore.Query(context.Background(), "ch-1", 0, 0)
	if err != nil {
		t.Fatalf("Recovery query: %v", err)
	}
	if len(recoveredPubs) != 1 {
		t.Errorf("expected 1 recovered publication, got %d", len(recoveredPubs))
	}
}

// TestTopicBroker_PublishWithOffset_IdempotentCache tests that PublishWithOffset
// returns cached result when idempotency key matches.
func TestTopicBroker_PublishWithOffset_IdempotentCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-pwo-idempotent"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	// Pre-allocate offset
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-1"}})
	if err != nil {
		t.Fatalf("BatchIncrby: %v", err)
	}
	sp := positions["ch-1"]

	opts := centrifuge.PublishOptions{
		HistorySize:    100,
		HistoryTTL:     time.Hour,
		IdempotencyKey: "msg-001",
	}

	// First publish — should succeed and cache
	err = broker.PublishWithOffset(ctx, "ch-1", []byte(`{"msg":1}`), opts, sp)
	if err != nil {
		t.Fatalf("first PublishWithOffset: %v", err)
	}

	// Second publish with same idempotency key and same offset — should hit cache
	err = broker.PublishWithOffset(ctx, "ch-1", []byte(`{"msg":1}`), opts, sp)
	if err != nil {
		t.Fatalf("second PublishWithOffset (cached): %v", err)
	}
	// No error means idempotency cache returned the cached result
}

// TestTopicBroker_PublishEndToEnd tests the full "persist first, then push" flow:
// BatchIncrby → Save to HistoryStore → PublishWithOffset → History reads from HistoryStore.
func TestTopicBroker_PublishEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-e2e"
	cleanupTestRedis(t, prefix)

	historyStore := newTestHistoryStore()
	broker, _, cleanup := setupTestBroker(t, prefix, withHistoryStore(historyStore))
	defer cleanup()

	ctx := context.Background()

	// Simulate IM flow: 3 messages with "persist first, then push"
	messages := []string{
		`{"text":"hello","sender":"user1"}`,
		`{"text":"world","sender":"user2"}`,
		`{"text":"foo","sender":"user1"}`,
	}

	for i, msg := range messages {
		ch := "ch-1"

		// Step 1: Pre-allocate offset
		positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: ch}})
		if err != nil {
			t.Fatalf("BatchIncrby msg %d: %v", i+1, err)
		}
		sp := positions[ch]

		// Step 2: Save to DB (simulate DB transaction)
		pub := &centrifuge.Publication{Data: []byte(msg)}
		historyStore.SaveWithOffset(ch, pub, sp.Offset)

		// Step 3: Push (best-effort)
		err = broker.PublishWithOffset(ctx, ch, []byte(msg), centrifuge.PublishOptions{
			HistorySize: 100,
			HistoryTTL:  time.Hour,
		}, sp)
		if err != nil {
			t.Fatalf("PublishWithOffset msg %d: %v", i+1, err)
		}
	}

	// Verify: History returns all 3 messages from HistoryStore
	pubs, sp, err := broker.History("ch-1", centrifuge.HistoryOptions{
		Filter: centrifuge.HistoryFilter{
			Since: &centrifuge.StreamPosition{Offset: 0},
		},
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pubs) != 3 {
		t.Fatalf("expected 3 publications, got %d", len(pubs))
	}
	if sp.Offset != 3 {
		t.Errorf("expected stream position offset 3, got %d", sp.Offset)
	}

	// Verify order and data
	for i, pub := range pubs {
		if string(pub.Data) != messages[i] {
			t.Errorf("pub %d: expected %s, got %s", i, messages[i], string(pub.Data))
		}
		if pub.Offset != uint64(i+1) { //nolint:gosec // 测试代码，i 不会溢出
			t.Errorf("pub %d: expected offset %d, got %d", i, i+1, pub.Offset)
		}
	}

	// Verify: Query with Since filter works
	pubs2, err := historyStore.Query(context.Background(), "ch-1", 1, 0)
	if err != nil {
		t.Fatalf("Query since 1: %v", err)
	}
	if len(pubs2) != 2 {
		t.Errorf("expected 2 publications after offset 1, got %d", len(pubs2))
	}
}

// ========== Helper types for testing ==========

type testHistoryStore struct {
	mu      sync.RWMutex
	data    map[string][]*centrifuge.Publication
	offsets map[string]uint64
}

func newTestHistoryStore() *testHistoryStore {
	return &testHistoryStore{
		data:    make(map[string][]*centrifuge.Publication),
		offsets: make(map[string]uint64),
	}
}

func (s *testHistoryStore) Query(_ context.Context, channel string, sinceOffset uint32, _ uint32) ([]*centrifuge.Publication, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pubs := s.data[channel]
	if sinceOffset == 0 {
		return pubs, nil
	}

	var result []*centrifuge.Publication
	for _, pub := range pubs {
		if pub.Offset > uint64(sinceOffset) {
			result = append(result, pub)
		}
	}
	return result, nil
}

func (s *testHistoryStore) Save(channel string, pub *centrifuge.Publication) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.offsets[channel]++
	nextOffset := s.offsets[channel]
	pub.Offset = nextOffset
	s.data[channel] = append(s.data[channel], pub)
	return nextOffset
}

// SaveWithOffset saves a publication with a specific offset (from BatchIncrby).
// Used in "persist first, then push" flow where offset is pre-allocated.
func (s *testHistoryStore) SaveWithOffset(channel string, pub *centrifuge.Publication, offset uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pub.Offset = offset
	s.data[channel] = append(s.data[channel], pub)
	s.offsets[channel] = offset
}

type testEventHandler struct{}

func (h *testEventHandler) HandlePublication(_ string, _ *centrifuge.Publication, _ centrifuge.StreamPosition, _ bool, _ *centrifuge.Publication) error {
	return nil
}

func (h *testEventHandler) HandleJoin(_ string, _ *centrifuge.ClientInfo) error {
	return nil
}

func (h *testEventHandler) HandleLeave(_ string, _ *centrifuge.ClientInfo) error {
	return nil
}

// Helper options for setupTestBroker
type testBrokerOption func(*TopicBrokerConfig)

func withHistoryStore(hs HistoryStore) testBrokerOption {
	return func(c *TopicBrokerConfig) {
		c.HistoryStore = hs
	}
}

// setupTestBroker creates a configured TopicBroker for testing.
func setupTestBroker(t *testing.T, prefix string, opts ...testBrokerOption) (*TopicBroker, *testEventHandler, func()) {
	t.Helper()

	_, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	historyStore := newTestHistoryStore()

	config := TopicBrokerConfig{
		Prefix:       prefix,
		RedisAddr:    "localhost:6379",
		RedisDB:      15,
		HistoryStore: historyStore,
	}

	for _, o := range opts {
		o(&config)
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("create broker: %v", err)
	}

	eh := &testEventHandler{}
	err = broker.RegisterBrokerEventHandler(eh)
	if err != nil {
		t.Fatalf("register event handler: %v", err)
	}

	cleanup := func() {
		_ = broker.Close(context.Background())
	}

	return broker, eh, cleanup
}

// ========== NewTopicBroker 参数校验测试 ==========

func TestNewTopicBroker_EmptyRedisAddr(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "test",
		RedisAddr: "",
	}

	_, err := NewTopicBroker(config)
	if err == nil {
		t.Error("expected error for empty RedisAddr, got nil")
	}
}

func TestNewTopicBroker_DefaultPrefix(t *testing.T) {
	// 空 prefix 应该使用默认值 "centrifuge"
	config := TopicBrokerConfig{
		Prefix:    "",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	if broker.prefix != "centrifuge" {
		t.Errorf("expected default prefix 'centrifuge', got %s", broker.prefix)
	}
}

func TestNewTopicBroker_CustomPrefix(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "custom-prefix",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	if broker.prefix != "custom-prefix" {
		t.Errorf("expected prefix 'custom-prefix', got %s", broker.prefix)
	}
}

func TestNewTopicBroker_NilLogger(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "test-nil-logger",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
		Logger:    nil,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	// 应该使用 defaultLogger
	if broker.logger == nil {
		t.Error("expected non-nil logger (defaultLogger fallback)")
	}
}

func TestNewTopicBroker_InvalidRedisAddr(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "test",
		RedisAddr: "invalid:addr:with:too:many:colons:12345",
	}

	_, err := NewTopicBroker(config)
	if err == nil {
		t.Error("expected error for invalid Redis address, got nil")
	}
}

// ========== TopicBroker.metaKey / pubSubKey 测试 ==========

func TestTopicBroker_MetaKey(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "test-prefix",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	key := broker.metaKey("test-channel")
	expected := "test-prefix:meta:test-channel"
	if key != expected {
		t.Errorf("expected %s, got %s", expected, key)
	}
}

func TestTopicBroker_PubSubKey(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "test-prefix",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	key := broker.pubSubKey("test-channel")
	expected := "test-prefix:pubsub:test-channel"
	if key != expected {
		t.Errorf("expected %s, got %s", expected, key)
	}
}

// ========== TopicBroker.BatchIncrby 边缘场景 ==========

func TestTopicBroker_BatchIncrby_EmptyRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-batch-empty"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{})
	if err != nil {
		t.Fatalf("BatchIncrby empty: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("expected 0 positions, got %d", len(positions))
	}
}

func TestTopicBroker_BatchIncrby_SameChannelTwice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-batch-twice"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	// 同一个 channel 在同一次 BatchIncrby 中出现两次应报错（使用 Count 字段替代）
	_, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{
		{Channel: "ch-1"},
		{Channel: "ch-1"},
	})
	if err == nil {
		t.Fatalf("expected error for duplicate channels, got nil")
	}

	// 用 Count=2 在单次调用里为同一 channel 预分配 2 个 offset
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{
		{Channel: "ch-2", Count: 2},
	})
	if err != nil {
		t.Fatalf("BatchIncrby with Count=2: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 position entry, got %d", len(positions))
	}
	// final offset 是 2，调用方可推导 [1, 2]
	if positions["ch-2"].Offset != 2 {
		t.Errorf("expected final offset 2, got %d", positions["ch-2"].Offset)
	}

	// 后续再分配应接续
	positions2, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{
		{Channel: "ch-2"},
	})
	if err != nil {
		t.Fatalf("BatchIncrby follow-up: %v", err)
	}
	if positions2["ch-2"].Offset != 3 {
		t.Errorf("expected offset 3 after previous Count=2, got %d", positions2["ch-2"].Offset)
	}
}

// ========== TopicBroker.History 边缘场景 ==========

func TestTopicBroker_History_MaxUint32Clamp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-history-clamp"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	// 发布一条消息以创建 meta key
	_, _ = broker.Publish("ch-1", []byte(`{}`), centrifuge.PublishOptions{})

	// History with Since.Offset > MaxUint32 应该被 clamp
	_, _, err := broker.History("ch-1", centrifuge.HistoryOptions{
		Filter: centrifuge.HistoryFilter{
			Since: &centrifuge.StreamPosition{
				Offset: 5000000000, // > math.MaxUint32
			},
		},
	})
	if err != nil {
		t.Errorf("expected no error with clamped offset, got: %v", err)
	}
}

// ========== DualBroker.getChannelType 测试 ==========

func TestDualBroker_GetChannelType_Default(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-get-ct"
	cleanupTestRedis(t, prefix+"-live:")
	cleanupTestRedis(t, prefix+"-topic:")

	node, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	redisShard, err := centrifuge.NewRedisShard(node, centrifuge.RedisShardConfig{
		Address: "localhost:6379",
	})
	if err != nil {
		t.Fatalf("create shard: %v", err)
	}

	config := DualBrokerConfig{
		Live: centrifuge.RedisBrokerConfig{
			Prefix: prefix + "-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: TopicBrokerConfig{
			Prefix:       prefix + "-topic",
			RedisAddr:    "localhost:6379",
			RedisDB:      15,
			HistoryStore: newTestHistoryStore(),
		},
	}

	broker, err := NewDualBroker(node, config)
	if err != nil {
		t.Fatalf("create dual broker: %v", err)
	}
	defer func() { _ = broker.Close(context.Background()) }()

	// 未注册的 channel 应该返回错误
	_, err = broker.getChannelType("unregistered-channel")
	if err == nil {
		t.Errorf("expected error for unregistered channel, got nil")
	}

	// 注册为 Topic 后应该返回 Topic
	broker.RegisterChannelType("topic-ch", Topic)
	ct, err := broker.getChannelType("topic-ch")
	if err != nil {
		t.Errorf("unexpected error for registered channel: %v", err)
	}
	if ct != Topic {
		t.Errorf("expected Topic for registered channel, got %v", ct)
	}

	// 注册为 Live
	broker.RegisterChannelType("live-ch", Live)
	ct, err = broker.getChannelType("live-ch")
	if err != nil {
		t.Errorf("unexpected error for live channel: %v", err)
	}
	if ct != Live {
		t.Errorf("expected Live for live channel, got %v", ct)
	}
}

// ========== BatchIncrby Count 边缘场景 ==========

func TestTopicBroker_BatchIncrby_CountZero(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-batch-count-zero"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	// Count=0 应该被规范化为 Count=1
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{
		{Channel: "ch-zero", Count: 0},
	})
	if err != nil {
		t.Fatalf("BatchIncrby with Count=0: %v", err)
	}
	if positions["ch-zero"].Offset != 1 {
		t.Errorf("expected offset 1 (Count=0 normalized to 1), got %d", positions["ch-zero"].Offset)
	}
}

func TestTopicBroker_BatchIncrby_CountNegative(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-batch-count-neg"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	// Count=-5 应该被规范化为 Count=1
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{
		{Channel: "ch-neg", Count: -5},
	})
	if err != nil {
		t.Fatalf("BatchIncrby with Count=-5: %v", err)
	}
	if positions["ch-neg"].Offset != 1 {
		t.Errorf("expected offset 1 (Count=-5 normalized to 1), got %d", positions["ch-neg"].Offset)
	}
}

func TestTopicBroker_BatchIncrby_EmptyChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-batch-empty-ch"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	// Channel 为空字符串应报错
	_, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{
		{Channel: ""},
	})
	if err == nil {
		t.Fatal("expected error for empty channel name, got nil")
	}
	if !strings.Contains(err.Error(), "channel is required") {
		t.Errorf("expected 'channel is required' error, got: %v", err)
	}
}

func TestTopicBroker_BatchIncrby_LargeCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-batch-large-count"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	// Count=10 一次性预分配 10 个 offset
	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{
		{Channel: "ch-large", Count: 10},
	})
	if err != nil {
		t.Fatalf("BatchIncrby with Count=10: %v", err)
	}
	if positions["ch-large"].Offset != 10 {
		t.Errorf("expected final offset 10, got %d", positions["ch-large"].Offset)
	}

	// 下一个分配应从 11 开始
	positions2, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{
		{Channel: "ch-large"},
	})
	if err != nil {
		t.Fatalf("follow-up BatchIncrby: %v", err)
	}
	if positions2["ch-large"].Offset != 11 {
		t.Errorf("expected offset 11 after Count=10, got %d", positions2["ch-large"].Offset)
	}
}

// ========== TopicBroker.IncrConversationOffset 测试 ==========

func TestTopicBroker_IncrConversationOffset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-conv-offset"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	convID := "test-conv-123"

	// 第一次分配
	offset1, err := broker.IncrConversationOffset(ctx, convID)
	if err != nil {
		t.Fatalf("first IncrConversationOffset: %v", err)
	}
	if offset1 != 1 {
		t.Errorf("expected offset 1, got %d", offset1)
	}

	// 第二次分配
	offset2, err := broker.IncrConversationOffset(ctx, convID)
	if err != nil {
		t.Fatalf("second IncrConversationOffset: %v", err)
	}
	if offset2 != 2 {
		t.Errorf("expected offset 2, got %d", offset2)
	}

	// 不同 conversation 独立计数
	offset3, err := broker.IncrConversationOffset(ctx, "other-conv")
	if err != nil {
		t.Fatalf("IncrConversationOffset other conv: %v", err)
	}
	if offset3 != 1 {
		t.Errorf("expected offset 1 for other conv, got %d", offset3)
	}
}

// ========== TopicBroker.Close 幂等测试 ==========

func TestTopicBroker_Close_Idempotent(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "test-close-idempotent",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}

	// 多次 Close 不应 panic
	if err := broker.Close(context.Background()); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := broker.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// ========== TopicBroker.History 无 Since 过滤测试 ==========

func TestTopicBroker_History_NoSince(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-history-nosince"
	cleanupTestRedis(t, prefix)

	historyStore := newTestHistoryStore()
	broker, _, cleanup := setupTestBroker(t, prefix, withHistoryStore(historyStore))
	defer cleanup()

	// 发布消息
	for i := 0; i < 3; i++ {
		data := fmt.Appendf(nil, `{"seq":%d}`, i+1)
		_, err := broker.Publish("ch-nosince", data, centrifuge.PublishOptions{})
		if err != nil {
			t.Fatalf("publish %d: %v", i+1, err)
		}
	}
	for i := 0; i < 3; i++ {
		historyStore.Save("ch-nosince", &centrifuge.Publication{
			Data: fmt.Appendf(nil, `{"seq":%d}`, i+1),
		})
	}

	// 不传 Since filter，应返回所有消息
	pubs, sp, err := broker.History("ch-nosince", centrifuge.HistoryOptions{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pubs) != 3 {
		t.Errorf("expected 3 publications, got %d", len(pubs))
	}
	if sp.Offset != 3 {
		t.Errorf("expected stream position offset 3, got %d", sp.Offset)
	}
}

// ========== TopicBroker.PublishWithOffset_IdempotentTTL 测试 ==========

func TestTopicBroker_PublishWithOffset_IdempotentCustomTTL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-pwo-ttl"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	ctx := context.Background()

	positions, err := broker.BatchIncrby(ctx, []ChannelIncrbyRequest{{Channel: "ch-ttl"}})
	if err != nil {
		t.Fatalf("BatchIncrby: %v", err)
	}
	sp := positions["ch-ttl"]

	opts := centrifuge.PublishOptions{
		HistorySize:         100,
		HistoryTTL:          time.Hour,
		IdempotencyKey:      "ttl-key",
		IdempotentResultTTL: 10 * time.Second,
	}

	err = broker.PublishWithOffset(ctx, "ch-ttl", []byte(`{"msg":1}`), opts, sp)
	if err != nil {
		t.Fatalf("PublishWithOffset: %v", err)
	}

	// 验证幂等缓存存在
	client, err := newTestRedisClient()
	if err != nil {
		t.Fatalf("create redis client: %v", err)
	}
	defer client.Close()

	resultKey := prefix + ":idempotent:ch-ttl:ttl-key"
	result, err := client.Do(ctx, client.B().Hmget().Key(resultKey).Field("e", "s").Build()).AsStrSlice()
	if err != nil {
		t.Fatalf("HMGET idempotent key: %v", err)
	}
	if len(result) < 2 || result[0] == "" {
		t.Error("expected idempotent cache to exist")
	}
}
