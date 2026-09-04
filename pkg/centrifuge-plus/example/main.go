// Package main 提供 centrifuge-plus 的示例应用，演示 TopicBroker 的使用方式。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/centrifugal/centrifuge"
	centrifugego "github.com/centrifugal/centrifuge-go"
	"github.com/redis/rueidis"
	centrifugeplus "github.com/rtc-agent/server/pkg/centrifuge-plus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

func initTracer(ctx context.Context) (*sdktrace.TracerProvider, error) {
	jaegerEndpoint := os.Getenv("JAEGER_ENDPOINT")
	if jaegerEndpoint == "" {
		jaegerEndpoint = "localhost:4317"
	}

	exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(
		otlptracegrpc.WithEndpoint(jaegerEndpoint),
		otlptracegrpc.WithInsecure(),
	))
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("centrifuge-plus-example"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp, nil
}

func main() {
	log.Println("=== centrifuge-plus Example ===")
	log.Println("使用方式：客户端通过 RPC 发送消息，服务端执行 BatchIncrby → 持久化 → PublishWithOffset")

	overallPass := true
	defer func() {
		if !overallPass {
			log.Println("=== Example FAILED ===")
			os.Exit(1)
		}
		log.Println("=== Example PASSED ===")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tp, err := initTracer(ctx)
	if err != nil {
		log.Fatalf("init tracer: %v", err)
	}
	defer func() {
		if shutdownErr := tp.Shutdown(context.Background()); shutdownErr != nil {
			log.Printf("Warning: tracer shutdown: %v", shutdownErr)
		}
	}()

	if err := cleanupRedis(ctx, redisAddr); err != nil {
		log.Printf("Warning: cleanup redis: %v", err)
	}

	node, err := centrifuge.New(centrifuge.Config{})
	if err != nil {
		log.Fatalf("create node: %v", err)
	}

	redisShard, err := centrifuge.NewRedisShard(node, centrifuge.RedisShardConfig{
		Address: redisAddr,
	})
	if err != nil {
		log.Fatalf("create shard: %v", err)
	}

	historyStore := newMemoryHistoryStore()

	broker, err := centrifugeplus.NewDualBroker(node, centrifugeplus.DualBrokerConfig{
		Live: centrifuge.RedisBrokerConfig{
			Prefix: "eg-live",
			Shards: []*centrifuge.RedisShard{redisShard},
		},
		Topic: centrifugeplus.TopicBrokerConfig{
			Prefix:       "eg-topic",
			RedisAddr:    redisAddr,
			RedisDB:      1,
			HistoryStore: historyStore,
			Tracing: centrifugeplus.TracingConfig{
				Enabled:  true,
				Provider: tp,
			},
		},
	})
	if err != nil {
		log.Fatalf("create broker: %v", err)
	}

	node.SetBroker(broker)

	// 预注册频道类型
	for _, ch := range []string{"group-1", "channel-1", "channel-2"} {
		broker.RegisterChannelType("topic:"+ch, centrifugeplus.Topic)
		broker.RegisterChannelType("live:"+ch, centrifugeplus.Live)
	}

	// 认证：使用 client ID 作为 user ID
	node.OnConnecting(func(_ context.Context, e centrifuge.ConnectEvent) (centrifuge.ConnectReply, error) {
		userID := e.Token
		if userID == "" {
			userID = e.ClientID
		}
		return centrifuge.ConnectReply{
			Credentials: &centrifuge.Credentials{UserID: userID},
		}, nil
	})

	node.OnConnect(func(client *centrifuge.Client) {
		// OnSubscribe: 注册 channel type + 开启 recovery
		client.OnSubscribe(func(e centrifuge.SubscribeEvent, cb centrifuge.SubscribeCallback) {
			ch := e.Channel
			if strings.HasPrefix(ch, "topic:") {
				broker.RegisterChannelType(ch, centrifugeplus.Topic)
				cb(centrifuge.SubscribeReply{
					Options: centrifuge.SubscribeOptions{
						EnableRecovery: true,
					},
				}, nil)
				return
			}
			if strings.HasPrefix(ch, "live:") {
				broker.RegisterChannelType(ch, centrifugeplus.Live)
			}
			cb(centrifuge.SubscribeReply{}, nil)
		})

		// OnRPC: 处理客户端发送消息的 RPC 调用
		// 这是正确的使用方式：客户端通过 RPC 发消息，服务端执行"先落盘再推送"
		client.OnRPC(func(e centrifuge.RPCEvent, cb centrifuge.RPCCallback) {
			switch e.Method {
			case "send.topic":
				handleSendTopic(ctx, broker, historyStore, tp, client, e, cb)
			default:
				cb(centrifuge.RPCReply{}, centrifuge.ErrorMethodNotFound)
			}
		})
	})

	if err := node.Run(); err != nil {
		log.Fatalf("run node: %v", err)
	}

	wsHandler := centrifuge.NewWebsocketHandler(node, centrifuge.WebsocketConfig{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	})

	mux := http.NewServeMux()
	mux.Handle("/connection/websocket", wsHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	httpServer := &http.Server{Addr: httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("HTTP server on %s", httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	if err := waitForServer(httpAddr, 5*time.Second); err != nil {
		log.Fatalf("server not ready: %v", err)
	}
	log.Println("=== Server Ready ===")

	time.Sleep(500 * time.Millisecond)

	rc, err := newRedisChecker(redisAddr, "eg-topic")
	if err != nil {
		log.Fatalf("create redis checker: %v", err)
	}
	defer rc.close()

	devices := []string{"user1-device-a", "user1-device-b", "user2-device-c", "user3-device-d"}
	var clients []*deviceClient
	for _, deviceID := range devices {
		dc, err := newDeviceClient(deviceID, serverURL)
		if err != nil {
			log.Fatalf("create client %s: %v", deviceID, err)
		}
		if err := dc.connect(ctx); err != nil {
			log.Fatalf("connect %s: %v", deviceID, err)
		}
		clients = append(clients, dc)
		log.Printf("Client connected: %s", deviceID)
	}
	time.Sleep(200 * time.Millisecond)

	// 订阅 Live 频道
	liveChannels := []string{"live:group-1", "live:channel-1", "live:channel-2"}
	for _, dc := range clients {
		for _, ch := range liveChannels {
			if err := dc.subscribe(ctx, ch); err != nil {
				log.Printf("  %s subscribe %s: %v", dc.id, ch, err)
			}
		}
	}
	time.Sleep(200 * time.Millisecond)

	// ========================================================================
	// Test 1: Live 消息（实时，不持久化）
	// ========================================================================
	log.Println("=== Test 1: Live messages (real-time, non-persisted) ===")
	// Live 消息使用 broker.PublishEphemeral 直接推送（模拟服务端主动推送场景）
	liveMsg1 := fmt.Sprintf(`{"text":"live message on group-1","ts":"%s"}`, time.Now().Format(time.RFC3339Nano))
	liveMsg2 := fmt.Sprintf(`{"text":"live message on channel-1","ts":"%s"}`, time.Now().Format(time.RFC3339Nano))
	liveMsg3 := fmt.Sprintf(`{"text":"live message on channel-2","ts":"%s"}`, time.Now().Format(time.RFC3339Nano))

	for _, dc := range clients {
		dc.clearMessages("live:group-1")
		dc.clearMessages("live:channel-1")
		dc.clearMessages("live:channel-2")
	}

	// 服务端直接通过 broker 推送 Live 消息
	_ = broker.PublishEphemeral(ctx, "live:group-1", []byte(liveMsg1), centrifuge.PublishOptions{})
	_ = broker.PublishEphemeral(ctx, "live:channel-1", []byte(liveMsg2), centrifuge.PublishOptions{})
	_ = broker.PublishEphemeral(ctx, "live:channel-2", []byte(liveMsg3), centrifuge.PublishOptions{})
	time.Sleep(500 * time.Millisecond)

	test1Pass := true
	for _, dc := range clients {
		for _, ch := range liveChannels {
			msgs := dc.receivedMessages(ch)
			if len(msgs) < 1 {
				log.Printf("  FAIL: %s did not receive message on %s", dc.id, ch)
				test1Pass = false
			}
		}
	}
	if test1Pass {
		log.Println("  PASS: All clients received all live messages")
	} else {
		overallPass = false
	}

	// ========================================================================
	// Test 2: Topic 消息（通过 RPC 发送，先落盘再推送）
	// ========================================================================
	log.Println("=== Test 2: Topic messages (RPC + persist first, then push) ===")
	topicChannels := []string{"topic:group-1", "topic:channel-1", "topic:channel-2"}
	for _, dc := range clients {
		for _, ch := range topicChannels {
			if err := dc.subscribe(ctx, ch); err != nil {
				log.Printf("  %s subscribe %s: %v", dc.id, ch, err)
			}
		}
	}
	time.Sleep(200 * time.Millisecond)

	for _, dc := range clients {
		for _, ch := range topicChannels {
			dc.clearMessages(ch)
		}
	}

	log.Println("  [redis-check] BEFORE topic publish:")
	for _, ch := range topicChannels {
		rc.checkMetaExists(ctx, ch)
	}

	// 通过 RPC 发送 Topic 消息（客户端 → 服务端 RPC handler → BatchIncrby → 持久化 → PublishWithOffset）
	for _, ch := range topicChannels {
		msg := fmt.Sprintf(`{"text":"topic message on %s","ts":"%s"}`, ch, time.Now().Format(time.RFC3339Nano))
		if err := clients[0].sendTopicRPC(ctx, ch, msg); err != nil {
			log.Printf("  FAIL: sendTopicRPC %s: %v", ch, err)
			overallPass = false
		}
	}
	time.Sleep(2 * time.Second)

	log.Println("  [redis-check] AFTER topic publish:")
	for _, ch := range topicChannels {
		rc.checkMetaExists(ctx, ch)
	}

	test2Pass := true
	for _, dc := range clients {
		for _, ch := range topicChannels {
			msgs := dc.receivedMessages(ch)
			if len(msgs) < 1 {
				log.Printf("  FAIL: %s did not receive topic message on %s", dc.id, ch)
				test2Pass = false
			}
		}
	}
	if test2Pass {
		log.Println("  PASS: All clients received all topic messages")
	} else {
		overallPass = false
	}

	// 验证 HistoryStore
	topicStoreOK := true
	for _, ch := range topicChannels {
		pubs, err := historyStore.Query(context.Background(), ch, 0, 0)
		if err != nil {
			log.Printf("  FAIL: historyStore.Query(%s): %v", ch, err)
			topicStoreOK = false
		} else if len(pubs) != 1 {
			log.Printf("  FAIL: historyStore.Query(%s) expected 1 publication, got %d", ch, len(pubs))
			topicStoreOK = false
		} else {
			log.Printf("  PASS: historyStore has publication for channel %s (offset=%d)", ch, pubs[0].Offset)
		}
	}
	if topicStoreOK {
		log.Println("  PASS: All topic messages persisted to history store")
	} else {
		overallPass = false
	}

	// ========================================================================
	// Test 3: 离线恢复
	// ========================================================================
	log.Println("=== Test 3: Offline recovery ===")
	offlineClient := clients[3]
	log.Printf("Disconnecting %s...", offlineClient.id)
	offlineClient.disconnect()
	time.Sleep(200 * time.Millisecond)

	for _, dc := range clients[:3] {
		for _, ch := range topicChannels {
			dc.clearMessages(ch)
		}
	}
	for _, ch := range topicChannels {
		offlineClient.clearMessages(ch)
	}

	// 发送离线消息
	for _, ch := range topicChannels {
		msg := fmt.Sprintf(`{"text":"offline message on %s","ts":"%s"}`, ch, time.Now().Format(time.RFC3339Nano))
		_ = clients[0].sendTopicRPC(ctx, ch, msg)
	}
	time.Sleep(3 * time.Second)

	log.Println("  [redis-check] AFTER offline message processing:")
	for _, ch := range topicChannels {
		rc.checkMetaExists(ctx, ch)
	}

	log.Printf("Reconnecting %s...", offlineClient.id)
	if err := offlineClient.connect(ctx); err != nil {
		log.Printf("  reconnect %s: %v", offlineClient.id, err)
	}
	if err := offlineClient.reSubscribeAll(ctx); err != nil {
		log.Printf("  re-subscribe %s: %v", offlineClient.id, err)
	}
	time.Sleep(3 * time.Second)

	for _, ch := range topicChannels {
		msgs := offlineClient.receivedMessages(ch)
		log.Printf("  %s received %d messages on %s after reconnect (expected >= 1)", offlineClient.id, len(msgs), ch)
	}
	afterReconnect := len(offlineClient.receivedMessages("topic:group-1"))
	if afterReconnect >= 1 {
		log.Println("  PASS: Offline client recovered messages after reconnection")
	} else {
		log.Println("  FAIL: Offline client did not recover any messages")
		overallPass = false
	}

	// 验证离线消息持久化
	offlineStoreOK := true
	for _, ch := range topicChannels {
		pubs, err := historyStore.Query(context.Background(), ch, 0, 0)
		if err != nil {
			log.Printf("  FAIL: historyStore.Query(%s): %v", ch, err)
			offlineStoreOK = false
		} else if len(pubs) < 2 {
			log.Printf("  FAIL: historyStore.Query(%s) expected >=2 publications, got %d", ch, len(pubs))
			offlineStoreOK = false
		} else {
			log.Printf("  PASS: channel %s has %d persisted publications (offset=%d..%d)", ch, len(pubs), pubs[0].Offset, pubs[len(pubs)-1].Offset)
		}
	}
	if offlineStoreOK {
		log.Println("  PASS: All offline messages persisted to history store")
	} else {
		overallPass = false
	}

	// ========================================================================
	// Test 4: Live 频道流式文本（打字机效果）
	// ========================================================================
	log.Println("=== Test 4: Streaming text on Live channels (typewriter effect) ===")
	streamCh := "live:group-1"
	for _, dc := range clients {
		dc.clearMessages(streamCh)
	}

	words := []string{"Hello", " ", "world", " ", "this", " ", "is", " ", "streaming", " ", "text"}
	for _, word := range words {
		msg := fmt.Sprintf(`{"text": "%s","stream":true}`, word)
		_ = broker.PublishEphemeral(ctx, streamCh, []byte(msg), centrifuge.PublishOptions{})
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	test4Pass := true
	for _, dc := range clients {
		msgs := dc.receivedMessages(streamCh)
		if len(msgs) < len(words) {
			log.Printf("  FAIL: %s received only %d/%d streaming words", dc.id, len(msgs), len(words))
			test4Pass = false
		}
	}
	if test4Pass {
		log.Println("  PASS: All clients received full streaming text")
	} else {
		overallPass = false
	}

	log.Println("=== Example complete ===")

	for _, dc := range clients {
		dc.disconnect()
		dc.close()
	}
	_ = httpServer.Shutdown(context.Background())
	_ = node.Shutdown(context.Background())
	_ = broker.Close(context.Background())
}

// handleSendTopic 处理 "send.topic" RPC：先落盘再推送
func handleSendTopic(
	ctx context.Context,
	broker *centrifugeplus.DualBroker,
	historyStore *memoryHistoryStore,
	tp *sdktrace.TracerProvider,
	_ *centrifuge.Client,
	e centrifuge.RPCEvent,
	cb centrifuge.RPCCallback,
) {
	var req struct {
		Channel string `json:"channel"`
		Data    string `json:"data"`
	}
	if err := json.Unmarshal(e.Data, &req); err != nil {
		cb(centrifuge.RPCReply{}, fmt.Errorf("invalid request: %w", err))
		return
	}

	ch := req.Channel
	if !strings.HasPrefix(ch, "topic:") {
		cb(centrifuge.RPCReply{}, fmt.Errorf("channel must start with topic: prefix"))
		return
	}

	workerTracer := tp.Tracer("example-persist")
	rpcCtx, span := workerTracer.Start(ctx, "example.send_topic",
		trace.WithAttributes(attribute.String("channel", ch)),
	)
	defer span.End()

	// Step 1: 预分配 offset（Redis HINCRBY）
	positions, err := broker.BatchIncrby(rpcCtx, []centrifugeplus.ChannelIncrbyRequest{{Channel: ch}})
	if err != nil {
		log.Printf("[sendTopic] BatchIncrby error: %v", err)
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		cb(centrifuge.RPCReply{}, fmt.Errorf("batch incrby: %w", err))
		return
	}
	sp := positions[ch]
	span.SetAttributes(
		attribute.Int64("offset", int64(sp.Offset)), //nolint:gosec // offset 不会超过 int64 范围
		attribute.String("epoch", sp.Epoch),
	)

	// Step 2: 持久化到 DB（此处用 memoryHistoryStore 模拟）
	pub := &centrifuge.Publication{Data: []byte(req.Data)}
	historyStore.SaveWithOffset(ch, pub, uint32(sp.Offset)) //nolint:gosec // offset 不会超过 uint32 范围
	log.Printf("[sendTopic] Persisted channel=%s offset=%d", ch, sp.Offset)

	// Step 3: 推送到 PUB/SUB（best-effort，失败不影响数据一致性）
	if err := broker.PublishWithOffset(rpcCtx, ch, []byte(req.Data), centrifuge.PublishOptions{
		HistorySize: 100,
		HistoryTTL:  24 * time.Hour,
	}, sp); err != nil {
		log.Printf("[sendTopic] PublishWithOffset error: %v", err)
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		// 推送失败但数据已持久化，客户端可通过主动拉取发现
	}

	cb(centrifuge.RPCReply{
		Data: fmt.Appendf(nil, `{"offset":%d}`, sp.Offset),
	}, nil)
}

// ============================================================================
// 客户端辅助类型
// ============================================================================

type deviceClient struct {
	id       string
	client   *centrifugego.Client
	mu       sync.Mutex
	received map[string][]string
	subs     map[string]*centrifugego.Subscription
}

func newDeviceClient(id string, endpoint string) (*deviceClient, error) {
	client := centrifugego.NewJsonClient(endpoint, centrifugego.Config{
		Token: id,
	})
	return &deviceClient{
		id:       id,
		client:   client,
		received: make(map[string][]string),
		subs:     make(map[string]*centrifugego.Subscription),
	}, nil
}

func (c *deviceClient) connect(_ context.Context) error {
	return c.client.Connect()
}

func (c *deviceClient) subscribe(_ context.Context, channel string) error {
	c.mu.Lock()
	if _, ok := c.subs[channel]; ok {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	sub, err := c.client.NewSubscription(channel)
	if err != nil {
		return fmt.Errorf("new sub: %w", err)
	}
	sub.OnPublication(func(e centrifugego.PublicationEvent) {
		c.mu.Lock()
		c.received[channel] = append(c.received[channel], string(e.Data))
		c.mu.Unlock()
	})
	if err := sub.Subscribe(); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	c.mu.Lock()
	c.subs[channel] = sub
	c.mu.Unlock()
	return nil
}

// sendTopicRPC 通过 RPC 发送 Topic 消息
func (c *deviceClient) sendTopicRPC(ctx context.Context, channel string, data string) error {
	req, _ := json.Marshal(map[string]string{
		"channel": channel,
		"data":    data,
	})
	_, err := c.client.RPC(ctx, "send.topic", req)
	return err
}

func (c *deviceClient) disconnect() {
	_ = c.client.Disconnect()
}

func (c *deviceClient) receivedMessages(channel string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]string, len(c.received[channel]))
	copy(result, c.received[channel])
	return result
}

func (c *deviceClient) reSubscribeAll(_ context.Context) error {
	c.mu.Lock()
	subs := make([]*centrifugego.Subscription, 0, len(c.subs))
	for _, sub := range c.subs {
		subs = append(subs, sub)
	}
	c.mu.Unlock()

	for _, sub := range subs {
		if err := sub.Subscribe(); err != nil {
			return fmt.Errorf("resubscribe: %w", err)
		}
	}
	return nil
}

func (c *deviceClient) clearMessages(channel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.received, channel)
}

func (c *deviceClient) close() {
	c.client.Close()
}

// ============================================================================
// MemoryHistoryStore（模拟 DB）
// ============================================================================

type memoryHistoryStore struct {
	mu      sync.RWMutex
	data    map[string][]*centrifuge.Publication
	offsets map[string]uint32
}

func newMemoryHistoryStore() *memoryHistoryStore {
	return &memoryHistoryStore{
		data:    make(map[string][]*centrifuge.Publication),
		offsets: make(map[string]uint32),
	}
}

func (s *memoryHistoryStore) Query(_ context.Context, channel string, sinceOffset uint32, _ uint32) ([]*centrifuge.Publication, error) {
	// latestOffset 在生产环境中用于补齐尾部缺口（fillGapPublications），
	// 保证恢复结果满足 centrifuge 的连续性检查（末条 pub.Offset == latestOffset）。
	// 示例程序是简化实现，未使用该参数。
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

func (s *memoryHistoryStore) RemoveHistory(channel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, channel)
	return nil
}

func (s *memoryHistoryStore) SaveWithOffset(channel string, pub *centrifuge.Publication, offset uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pub.Offset = uint64(offset)
	s.data[channel] = append(s.data[channel], pub)
	s.offsets[channel] = offset
}

// ============================================================================
// Redis checker（调试辅助）
// ============================================================================

type redisChecker struct {
	client rueidis.Client
	prefix string
}

func newRedisChecker(addr string, prefix string) (*redisChecker, error) {
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{addr},
		SelectDB:    1,
	})
	if err != nil {
		return nil, err
	}
	return &redisChecker{client: client, prefix: prefix}, nil
}

func (rc *redisChecker) close() {
	rc.client.Close()
}

func (rc *redisChecker) checkMetaExists(ctx context.Context, channel string) {
	metaKey := rc.prefix + ":meta:" + channel
	exists, _ := rc.client.Do(ctx, rc.client.B().Exists().Key(metaKey).Build()).AsInt64()
	if exists > 0 {
		val, _ := rc.client.Do(ctx, rc.client.B().Hmget().Key(metaKey).Field("s", "e").Build()).AsStrSlice()
		log.Printf("  [redis-check] meta %s: exists, offset=%s epoch=%s", metaKey, val[0], val[1])
	} else {
		log.Printf("  [redis-check] meta %s: NOT FOUND", metaKey)
	}
}
