package centrifugeplus

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/centrifugal/centrifuge"
	"github.com/redis/rueidis"
)

// ========== RegisterBrokerEventHandler concurrency-safety tests ==========
// Run with -race flag to verify there is no data race.

func TestTopicBroker_RegisterBrokerEventHandler_ConcurrentRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-race-register"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	// Concurrently register and read the eventHandler; verify no data race.
	var wg sync.WaitGroup
	numGoroutines := 50

	for i := range numGoroutines {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			eh := &testEventHandler{}
			_ = broker.RegisterBrokerEventHandler(eh)
		}(i)
	}

	// Concurrently read the eventHandler (indirectly via handlePubSubMessage).
	for range numGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Read the eventHandler pointer directly, simulating handlePubSubMessage.
			h := broker.eventHandler.Load()
			_ = h
		}()
	}

	wg.Wait()
}

func TestTopicBroker_RegisterBrokerEventHandler_ConcurrentRegisterAndPubSub(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-race-reg-pubsub"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	var wg sync.WaitGroup
	numGoroutines := 30

	// Concurrently register handlers.
	for i := range numGoroutines {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			eh := &testEventHandler{}
			_ = broker.RegisterBrokerEventHandler(eh)
		}(i)
	}

	// Concurrently simulate the eventHandler.Load() call of handlePubSubMessage.
	for range numGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Simulate the eventHandler.Load() call inside handlePubSubMessage.
			h := broker.eventHandler.Load()
			if h != nil {
				// Safe dereference.
				_ = *h
			}
		}()
	}

	wg.Wait()
}

// ========== handlePubSubMessage does not panic when eventHandler is unregistered ==========

func TestTopicBroker_HandlePubSubMessage_NoEventHandler(t *testing.T) {
	// Do not register an eventHandler; call handlePubSubMessage directly and verify no panic.
	config := TopicBrokerConfig{
		Prefix:    "test-no-handler",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	// eventHandler is unregistered (nil); handlePubSubMessage should return safely.
	// Construct a synthetic PUB/SUB message.
	msg := rueidis.PubSubMessage{
		Channel: "test-no-handler:pubsub:test-channel",
		Message: "__p1:1:test-epoch:11__hello data",
	}

	// Should not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("handlePubSubMessage panicked with nil eventHandler: %v", r)
		}
	}()

	broker.handlePubSubMessage(msg)
}

func TestTopicBroker_HandlePubSubMessage_NoEventHandler_JoinMessage(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "test-no-handler-join",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	msg := rueidis.PubSubMessage{
		Channel: "test-no-handler-join:pubsub:test-channel",
		Message: `__j1:{"user_id":"test-user","client_id":"test-client"}`,
	}

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("handlePubSubMessage panicked with nil eventHandler (join): %v", r)
		}
	}()

	broker.handlePubSubMessage(msg)
}

func TestTopicBroker_HandlePubSubMessage_NoEventHandler_LeaveMessage(t *testing.T) {
	config := TopicBrokerConfig{
		Prefix:    "test-no-handler-leave",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	msg := rueidis.PubSubMessage{
		Channel: "test-no-handler-leave:pubsub:test-channel",
		Message: `__l1:{"user_id":"test-user","client_id":"test-client"}`,
	}

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("handlePubSubMessage panicked with nil eventHandler (leave): %v", r)
		}
	}()

	broker.handlePubSubMessage(msg)
}

func TestTopicBroker_HandlePubSubMessage_UnknownPrefix(t *testing.T) {
	// Verify safe return when the channel prefix does not match.
	config := TopicBrokerConfig{
		Prefix:    "test-unknown-prefix",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	// Register a handler.
	eh := &testEventHandler{}
	_ = broker.RegisterBrokerEventHandler(eh)

	// Channel prefix does not match (not the pubsub: prefix).
	msg := rueidis.PubSubMessage{
		Channel: "wrong-prefix:test-channel",
		Message: "__p1:1:epoch:5__hello",
	}

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("handlePubSubMessage panicked with wrong prefix: %v", r)
		}
	}()

	broker.handlePubSubMessage(msg)
}

func TestTopicBroker_HandlePubSubMessage_InvalidPayload(t *testing.T) {
	// Verify safe return (no panic) on invalid payloads.
	config := TopicBrokerConfig{
		Prefix:    "test-invalid-payload",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	eh := &testEventHandler{}
	_ = broker.RegisterBrokerEventHandler(eh)

	tests := []struct {
		name    string
		payload string
	}{
		{"empty payload", ""},
		{"unknown prefix", "__x1:invalid"},
		{"malformed p1 meta", "__p1:invalid__data"},
		{"p1 wrong meta parts count", "__p1:1:epoch__data"},
		{"p1 negative dataLen", "__p1:1:epoch:-5__data"},
		{"p1 dataLen exceeds data", "__p1:1:epoch:100__short"},
		{"malformed j1 json", "__j1:{invalid"},
		{"malformed l1 json", "__l1:{invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := rueidis.PubSubMessage{
				Channel: "test-invalid-payload:pubsub:test-channel",
				Message: tt.payload,
			}

			defer func() {
				if r := recover(); r != nil {
					t.Errorf("handlePubSubMessage panicked for %q: %v", tt.payload, r)
				}
			}()

			broker.handlePubSubMessage(msg)
		})
	}
}

// ========== handlePubSubMessage success-path tests ==========

// recordingEventHandler records calls so we can verify handlePubSubMessage
// forwards messages correctly.
type recordingEventHandler struct {
	mu           sync.Mutex
	publications []*centrifuge.Publication
	joins        []*centrifuge.ClientInfo
	leaves       []*centrifuge.ClientInfo
}

func (r *recordingEventHandler) HandlePublication(_ string, pub *centrifuge.Publication, _ centrifuge.StreamPosition, _ bool, _ *centrifuge.Publication) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publications = append(r.publications, pub)
	return nil
}

func (r *recordingEventHandler) HandleJoin(_ string, info *centrifuge.ClientInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.joins = append(r.joins, info)
	return nil
}

func (r *recordingEventHandler) HandleLeave(_ string, info *centrifuge.ClientInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaves = append(r.leaves, info)
	return nil
}

func TestTopicBroker_HandlePubSubMessage_SuccessfulPublication(t *testing.T) {
	// Verify a successful publication message is forwarded to the eventHandler.
	config := TopicBrokerConfig{
		Prefix:    "test-success-pub",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	recorder := &recordingEventHandler{}
	_ = broker.RegisterBrokerEventHandler(recorder)

	// Construct a well-formed publication message: __p1:{offset}:{epoch}:{data_len}__{data}
	data := `{"text":"hello"}`
	payload := fmt.Sprintf("__p1:42:test-epoch:%d__%s", len(data), data)

	msg := rueidis.PubSubMessage{
		Channel: "test-success-pub:pubsub:my-channel",
		Message: payload,
	}

	broker.handlePubSubMessage(msg)

	if len(recorder.publications) != 1 {
		t.Fatalf("expected 1 publication, got %d", len(recorder.publications))
	}

	pub := recorder.publications[0]
	if pub.Offset != 42 {
		t.Errorf("expected offset 42, got %d", pub.Offset)
	}
	if string(pub.Data) != data {
		t.Errorf("expected data %q, got %q", data, string(pub.Data))
	}
}

func TestTopicBroker_HandlePubSubMessage_SuccessfulJoin(t *testing.T) {
	// Verify the join message is parsed and forwarded correctly.
	config := TopicBrokerConfig{
		Prefix:    "test-success-join",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	recorder := &recordingEventHandler{}
	_ = broker.RegisterBrokerEventHandler(recorder)

	payload := `__j1:{"UserID":"user-1","ClientID":"client-1"}`
	msg := rueidis.PubSubMessage{
		Channel: "test-success-join:pubsub:ch",
		Message: payload,
	}

	broker.handlePubSubMessage(msg)

	if len(recorder.joins) != 1 {
		t.Fatalf("expected 1 join, got %d", len(recorder.joins))
	}
	if recorder.joins[0].UserID != "user-1" {
		t.Errorf("expected user_id 'user-1', got %q", recorder.joins[0].UserID)
	}
}

func TestTopicBroker_HandlePubSubMessage_SuccessfulLeave(t *testing.T) {
	// Verify the leave message is parsed and forwarded correctly.
	config := TopicBrokerConfig{
		Prefix:    "test-success-leave",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	recorder := &recordingEventHandler{}
	_ = broker.RegisterBrokerEventHandler(recorder)

	payload := `__l1:{"UserID":"user-2","ClientID":"client-2"}`
	msg := rueidis.PubSubMessage{
		Channel: "test-success-leave:pubsub:ch",
		Message: payload,
	}

	broker.handlePubSubMessage(msg)

	if len(recorder.leaves) != 1 {
		t.Fatalf("expected 1 leave, got %d", len(recorder.leaves))
	}
	if recorder.leaves[0].UserID != "user-2" {
		t.Errorf("expected user_id 'user-2', got %q", recorder.leaves[0].UserID)
	}
}

func TestTopicBroker_HandlePubSubMessage_MissingSeparator(t *testing.T) {
	// Test the scenario where the __p1: format is missing the __ separator.
	config := TopicBrokerConfig{
		Prefix:    "test-missing-sep",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	recorder := &recordingEventHandler{}
	_ = broker.RegisterBrokerEventHandler(recorder)

	// After __p1: there is meta but no __ separator.
	msg := rueidis.PubSubMessage{
		Channel: "test-missing-sep:pubsub:ch",
		Message: "__p1:1:epoch:5data_without_separator",
	}

	broker.handlePubSubMessage(msg)

	// Should return safely without invoking the handler.
	if len(recorder.publications) != 0 {
		t.Errorf("expected 0 publications when separator missing, got %d", len(recorder.publications))
	}
}

func TestTopicBroker_HandlePubSubMessage_DataLenZero(t *testing.T) {
	// Test the dataLen=0 edge case (empty but valid data).
	config := TopicBrokerConfig{
		Prefix:    "test-datalen-zero",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	recorder := &recordingEventHandler{}
	_ = broker.RegisterBrokerEventHandler(recorder)

	// dataLen=0; encodedData is the empty string.
	payload := "__p1:1:epoch:0__"
	msg := rueidis.PubSubMessage{
		Channel: "test-datalen-zero:pubsub:ch",
		Message: payload,
	}

	broker.handlePubSubMessage(msg)

	if len(recorder.publications) != 1 {
		t.Fatalf("expected 1 publication for dataLen=0, got %d", len(recorder.publications))
	}
	if len(recorder.publications[0].Data) != 0 {
		t.Errorf("expected empty data, got %q", string(recorder.publications[0].Data))
	}
}

func TestTopicBroker_HandlePubSubMessage_ChannelPrefixStripping(t *testing.T) {
	// Verify the channel prefix is stripped before being passed to the handler.
	config := TopicBrokerConfig{
		Prefix:    "test-prefix-strip",
		RedisAddr: "localhost:6379",
		RedisDB:   15,
	}

	broker, err := NewTopicBroker(config)
	if err != nil {
		t.Fatalf("NewTopicBroker: %v", err)
	}
	defer func() { _ = broker.Close(context.TODO()) }()

	// Use channelCapturingHandler to verify the channel argument.
	handler := &channelCapturingHandler{}
	_ = broker.RegisterBrokerEventHandler(handler)

	data := "test"
	payload := fmt.Sprintf("__p1:1:e:%d__%s", len(data), data)
	msg := rueidis.PubSubMessage{
		Channel: "test-prefix-strip:pubsub:original-channel",
		Message: payload,
	}

	broker.handlePubSubMessage(msg)

	if handler.capturedChannel != "original-channel" {
		t.Errorf("expected channel 'original-channel', got %q", handler.capturedChannel)
	}
}

// channelCapturingHandler captures the channel name passed to the handler.
type channelCapturingHandler struct {
	capturedChannel string
}

func (h *channelCapturingHandler) HandlePublication(ch string, _ *centrifuge.Publication, _ centrifuge.StreamPosition, _ bool, _ *centrifuge.Publication) error {
	h.capturedChannel = ch
	return nil
}

func (h *channelCapturingHandler) HandleJoin(ch string, _ *centrifuge.ClientInfo) error {
	h.capturedChannel = ch
	return nil
}

func (h *channelCapturingHandler) HandleLeave(ch string, _ *centrifuge.ClientInfo) error {
	h.capturedChannel = ch
	return nil
}

// ========== TopicBroker.Close cleanup tests ==========
// Note: TestTopicBroker_Close_Idempotent is covered in broker_test.go.

func TestTopicBroker_Close_ClearsSubscribedChans(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-close-chans"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	// Manually subscribe to a few channels.
	if err := broker.Subscribe("ch-1"); err != nil {
		t.Fatalf("Subscribe ch-1: %v", err)
	}
	if err := broker.Subscribe("ch-2"); err != nil {
		t.Fatalf("Subscribe ch-2: %v", err)
	}

	// Verify subscribedChans is non-empty.
	broker.pubSubMu.Lock()
	countBefore := len(broker.subscribedChans)
	broker.pubSubMu.Unlock()

	if countBefore != 2 {
		t.Fatalf("expected 2 subscribed chans before Close, got %d", countBefore)
	}

	// Call Close.
	ctx := context.Background()
	if err := broker.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify subscribedChans has been cleared.
	broker.pubSubMu.Lock()
	countAfter := len(broker.subscribedChans)
	pubSubClient := broker.pubSubClient
	broker.pubSubMu.Unlock()

	if countAfter != 0 {
		t.Errorf("expected 0 subscribed chans after Close, got %d", countAfter)
	}
	if pubSubClient != nil {
		t.Error("expected pubSubClient to be nil after Close")
	}
}

// After Close the redisClient is shut down, so Subscribe must return an
// error (and must not panic).
func TestTopicBroker_Close_SubscribeAfterClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-close-resub"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	// Subscribe first, then close.
	_ = broker.Subscribe("ch-before-close")
	_ = broker.Close(context.Background())

	// After Close, Subscribe must return an error (redisClient is closed) and must not panic.
	err := broker.Subscribe("ch-after-close")
	if err == nil {
		t.Fatal("expected error when subscribing after Close, got nil")
	}

	// subscribedChans must remain empty.
	broker.pubSubMu.Lock()
	count := len(broker.subscribedChans)
	broker.pubSubMu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 subscribed chans after failed re-subscribe, got %d", count)
	}
}
