package svc

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// centrifugeMetrics holds all Prometheus metrics for the Centrifuge subsystem.
// All collectors are registered automatically via promauto and are safe for
// concurrent use.
type centrifugeMetrics struct {
	connectionsTotal   *prometheus.CounterVec   // labels: status (connected|disconnected)
	disconnectReason   *prometheus.CounterVec   // labels: reason (slow|normal|error)
	rpcRequestsTotal   *prometheus.CounterVec   // labels: method, status (success|error)
	rpcDuration        *prometheus.HistogramVec // labels: method
	subscriptionsTotal *prometheus.CounterVec   // labels: channel_type (user|topic|live)
	connectingDuration *prometheus.HistogramVec // labels: status (success|error)
}

// newCentrifugeMetrics creates and registers all Centrifuge metrics.
func newCentrifugeMetrics() *centrifugeMetrics {
	return &centrifugeMetrics{
		connectionsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "centrifuge",
			Name:      "connections_total",
			Help:      "Total number of Centrifuge connection events.",
		}, []string{"status"}), // status: "connected", "disconnected"

		disconnectReason: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "centrifuge",
			Name:      "disconnect_reason_total",
			Help:      "Total number of disconnections by reason.",
		}, []string{"reason"}), // reason: "slow", "normal", "error"

		rpcRequestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "centrifuge",
			Name:      "rpc_requests_total",
			Help:      "Total number of RPC requests handled by Centrifuge.",
		}, []string{"method", "status"}), // status: "success", "error"

		rpcDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "centrifuge",
			Name:      "rpc_duration_seconds",
			Help:      "RPC request handling duration in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms ~ 4s
		}, []string{"method"}),

		subscriptionsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "centrifuge",
			Name:      "subscriptions_total",
			Help:      "Total number of channel subscriptions.",
		}, []string{"channel_type"}), // channel_type: "user", "topic", "live"

		connectingDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "centrifuge",
			Name:      "connecting_duration_seconds",
			Help:      "OnConnecting (JWT verification) duration in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 10), // 1ms ~ 512ms
		}, []string{"status"}), // status: "success", "error"
	}
}

// recordConnection records a connection event.
func (m *centrifugeMetrics) recordConnected() {
	m.connectionsTotal.WithLabelValues("connected").Inc()
}

// recordDisconnection records a disconnection event with the reason.
func (m *centrifugeMetrics) recordDisconnected(reason string) {
	m.connectionsTotal.WithLabelValues("disconnected").Inc()
	m.disconnectReason.WithLabelValues(reason).Inc()
}

// recordRPC records an RPC request with its method, status, and duration.
func (m *centrifugeMetrics) recordRPC(method, status string, duration time.Duration) {
	m.rpcRequestsTotal.WithLabelValues(method, status).Inc()
	m.rpcDuration.WithLabelValues(method).Observe(duration.Seconds())
}

// recordSubscription records a channel subscription.
func (m *centrifugeMetrics) recordSubscription(channelType string) {
	m.subscriptionsTotal.WithLabelValues(channelType).Inc()
}

// recordConnecting records the OnConnecting (JWT verification) duration.
func (m *centrifugeMetrics) recordConnecting(status string, duration time.Duration) {
	m.connectingDuration.WithLabelValues(status).Observe(duration.Seconds())
}
