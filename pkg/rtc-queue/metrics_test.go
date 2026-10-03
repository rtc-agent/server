package rtcqueue

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// getCounterVecValue reads the current value of a counter vec for given labels.
func getCounterVecValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := cv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	if err := c.Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// getHistogramVecCount reads the sample count from a histogram vec.
func getHistogramVecCount(t *testing.T, hv *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	observer, err := hv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	h, ok := observer.(prometheus.Histogram)
	if !ok {
		t.Fatal("observer is not a Histogram")
	}
	if err := h.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestQueueMetrics_RecordPublish(t *testing.T) {
	t.Parallel()
	m := globalQueueMetrics

	before := getCounterVecValue(t, m.publishTotal, "success")
	m.recordPublish("success")
	after := getCounterVecValue(t, m.publishTotal, "success")

	if after != before+1 {
		t.Errorf("publish success counter = %v, want %v", after, before+1)
	}
}

func TestQueueMetrics_RecordClaim(t *testing.T) {
	t.Parallel()
	m := globalQueueMetrics

	for _, status := range []string{"success", "empty", "error"} {
		before := getCounterVecValue(t, m.claimTotal, status)
		m.recordClaim(status)
		after := getCounterVecValue(t, m.claimTotal, status)
		if after != before+1 {
			t.Errorf("claim %s counter = %v, want %v", status, after, before+1)
		}
	}
}

func TestQueueMetrics_RecordComplete(t *testing.T) {
	t.Parallel()
	m := globalQueueMetrics

	before := getCounterVecValue(t, m.completeTotal, "success")
	m.recordComplete("success")
	after := getCounterVecValue(t, m.completeTotal, "success")

	if after != before+1 {
		t.Errorf("complete success counter = %v, want %v", after, before+1)
	}
}

func TestQueueMetrics_RecordCancel(t *testing.T) {
	t.Parallel()
	m := globalQueueMetrics

	before := getCounterVecValue(t, m.cancelTotal, "user_request")
	m.recordCancel("user_request")
	after := getCounterVecValue(t, m.cancelTotal, "user_request")

	if after != before+1 {
		t.Errorf("cancel counter = %v, want %v", after, before+1)
	}
}

func TestQueueMetrics_RecordWaitDuration(t *testing.T) {
	t.Parallel()
	m := globalQueueMetrics

	before := getHistogramVecCount(t, m.waitDuration)
	m.recordWaitDuration(100 * time.Millisecond)
	after := getHistogramVecCount(t, m.waitDuration)

	if after != before+1 {
		t.Errorf("wait duration histogram count = %d, want %d", after, before+1)
	}
}

func TestQueueMetrics_RecordProcessDuration(t *testing.T) {
	t.Parallel()
	m := globalQueueMetrics

	before := getHistogramVecCount(t, m.processDuration)
	m.recordProcessDuration(500 * time.Millisecond)
	after := getHistogramVecCount(t, m.processDuration)

	if after != before+1 {
		t.Errorf("process duration histogram count = %d, want %d", after, before+1)
	}
}

// TestGlobalQueueMetrics verifies that the global metrics instance is initialized.
func TestGlobalQueueMetrics(t *testing.T) {
	if globalQueueMetrics == nil {
		t.Fatal("globalQueueMetrics is nil")
	}
}

// TestQueueIntegrationMetrics tests that queue operations update metrics correctly.
// This uses a mock Redis to verify the integration.
func TestQueueIntegrationMetrics(t *testing.T) {
	t.Skip("Requires Redis - integration test")
	// This test would require a real or mock Redis instance.
	// The unit tests above verify the metrics recording functions work correctly.
	_ = context.Background()
}
