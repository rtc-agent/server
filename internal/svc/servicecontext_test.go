package svc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Use a single metrics instance for all tests to avoid duplicate promauto registration.
var testMetrics = newCentrifugeMetrics()

func TestNewCentrifugeMetrics(t *testing.T) {
	t.Parallel()

	m := newCentrifugeMetrics()
	assert.NotNil(t, m)
	assert.NotNil(t, m.connectionsTotal)
	assert.NotNil(t, m.disconnectReason)
	assert.NotNil(t, m.rpcRequestsTotal)
	assert.NotNil(t, m.rpcDuration)
	assert.NotNil(t, m.subscriptionsTotal)
	assert.NotNil(t, m.connectingDuration)
}

func TestCentrifugeMetrics_RecordConnected(t *testing.T) {
	assert.NotPanics(t, func() {
		testMetrics.recordConnected()
	})
}

func TestCentrifugeMetrics_RecordDisconnected(t *testing.T) {
	assert.NotPanics(t, func() {
		testMetrics.recordDisconnected("slow")
		testMetrics.recordDisconnected("normal")
		testMetrics.recordDisconnected("error")
	})
}

func TestCentrifugeMetrics_RecordRPC(t *testing.T) {
	assert.NotPanics(t, func() {
		testMetrics.recordRPC("v1.session.list", "success", 10*time.Millisecond)
		testMetrics.recordRPC("v1.message.send", "error", 50*time.Millisecond)
	})
}

func TestCentrifugeMetrics_RecordSubscription(t *testing.T) {
	assert.NotPanics(t, func() {
		testMetrics.recordSubscription("user")
		testMetrics.recordSubscription("topic")
		testMetrics.recordSubscription("live")
	})
}

func TestCentrifugeMetrics_RecordConnecting(t *testing.T) {
	assert.NotPanics(t, func() {
		testMetrics.recordConnecting("success", 5*time.Millisecond)
		testMetrics.recordConnecting("error", 20*time.Millisecond)
	})
}

func TestCreateCentrifugeLogHandler(t *testing.T) {
	t.Parallel()

	handler := createCentrifugeLogHandler()
	assert.NotNil(t, handler)
}
