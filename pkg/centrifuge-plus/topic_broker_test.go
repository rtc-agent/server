package centrifugeplus

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/centrifugal/centrifuge"
	"github.com/redis/rueidis"
)

// ========== RegisterBrokerEventHandler 并发安全测试 ==========
// 使用 -race flag 运行时验证无 data race

func TestTopicBroker_RegisterBrokerEventHandler_ConcurrentRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-race-register"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	// 并发注册和读取 eventHandler，验证无 data race
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

	// 同时并发读取 eventHandler（通过 handlePubSubMessage 间接读取）
	for range numGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 直接读取 eventHandler 指针，模拟 handlePubSubMessage 的行为
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

	// 并发注册 handler
	for i := range numGoroutines {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			eh := &testEventHandler{}
			_ = broker.RegisterBrokerEventHandler(eh)
		}(i)
	}

	// 并发模拟 handlePubSubMessage 的 eventHandler.Load() 操作
	for range numGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 模拟 handlePubSubMessage 中 eventHandler.Load() 的调用
			h := broker.eventHandler.Load()
			if h != nil {
				// 安全地解引用
				_ = *h
			}
		}()
	}

	wg.Wait()
}

// ========== handlePubSubMessage 未注册 eventHandler 时不 panic ==========

func TestTopicBroker_HandlePubSubMessage_NoEventHandler(t *testing.T) {
	// 不注册 eventHandler，直接调用 handlePubSubMessage 验证不 panic
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

	// eventHandler 未注册（nil），handlePubSubMessage 应该安全返回
	// 构造一个模拟的 PUB/SUB 消息
	msg := rueidis.PubSubMessage{
		Channel: "test-no-handler:pubsub:test-channel",
		Message: "__p1:1:test-epoch:11__hello data",
	}

	// 应该不 panic
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
	// 测试 channel 前缀不匹配时安全返回
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

	// 注册一个 handler
	eh := &testEventHandler{}
	_ = broker.RegisterBrokerEventHandler(eh)

	// channel 前缀不匹配（非 pubsub: 前缀）
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
	// 测试无效 payload 时安全返回（不 panic）
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

// ========== handlePubSubMessage 成功路径测试 ==========

// recordingEventHandler 记录调用，用于验证 handlePubSubMessage 正确转发消息
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
	// 验证成功的 publication 消息被正确转发到 eventHandler
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

	// 构造合法的 publication 消息：__p1:{offset}:{epoch}:{data_len}__{data}
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
	// 验证 join 消息被正确解析并转发
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
	// 验证 leave 消息被正确解析并转发
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
	// 测试 __p1: 格式中缺少 __ 分隔符的场景
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

	// __p1: 后有 meta 但没有 __ 分隔符
	msg := rueidis.PubSubMessage{
		Channel: "test-missing-sep:pubsub:ch",
		Message: "__p1:1:epoch:5data_without_separator",
	}

	broker.handlePubSubMessage(msg)

	// 应该安全返回，不调用 handler
	if len(recorder.publications) != 0 {
		t.Errorf("expected 0 publications when separator missing, got %d", len(recorder.publications))
	}
}

func TestTopicBroker_HandlePubSubMessage_DataLenZero(t *testing.T) {
	// 测试 dataLen=0 的边界情况（空数据但合法）
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

	// dataLen=0，encodedData 为空字符串
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
	// 验证 channel 前缀被正确剥离后传给 handler
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

	// 使用 channelCapturingHandler 验证 channel 参数
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

// channelCapturingHandler 捕获传递给 handler 的 channel 名称
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

// ========== TopicBroker.Close 清理测试 ==========
// 注：TestTopicBroker_Close_Idempotent 已在 broker_test.go 中覆盖

func TestTopicBroker_Close_ClearsSubscribedChans(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-close-chans"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	// 手动订阅一些频道
	if err := broker.Subscribe("ch-1"); err != nil {
		t.Fatalf("Subscribe ch-1: %v", err)
	}
	if err := broker.Subscribe("ch-2"); err != nil {
		t.Fatalf("Subscribe ch-2: %v", err)
	}

	// 验证 subscribedChans 非空
	broker.pubSubMu.Lock()
	countBefore := len(broker.subscribedChans)
	broker.pubSubMu.Unlock()

	if countBefore != 2 {
		t.Fatalf("expected 2 subscribed chans before Close, got %d", countBefore)
	}

	// 调用 Close
	ctx := context.Background()
	if err := broker.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 验证 subscribedChans 已被清空
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

// Close 后 redisClient 已关闭，Subscribe 应返回错误（不会 panic）
func TestTopicBroker_Close_SubscribeAfterClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	prefix := "test-close-resub"
	cleanupTestRedis(t, prefix)

	broker, _, cleanup := setupTestBroker(t, prefix)
	defer cleanup()

	// 先订阅再关闭
	_ = broker.Subscribe("ch-before-close")
	_ = broker.Close(context.Background())

	// Close 后 Subscribe 应返回错误（redisClient 已关闭），不应 panic
	err := broker.Subscribe("ch-after-close")
	if err == nil {
		t.Fatal("expected error when subscribing after Close, got nil")
	}

	// subscribedChans 应保持为空
	broker.pubSubMu.Lock()
	count := len(broker.subscribedChans)
	broker.pubSubMu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 subscribed chans after failed re-subscribe, got %d", count)
	}
}
