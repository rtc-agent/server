package turnagent

import (
	"context"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// sharedMetrics ensures we only register Prometheus metrics once across all tests.
var sharedMetrics = sync.OnceValue(func() *PrometheusMetrics {
	return NewPrometheusMetrics()
})

// getCounterValue reads the current value of a counter vec for given labels.
func getCounterValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
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

// getHistogramCount reads the sample count from a histogram vec for given labels.
func getHistogramCount(t *testing.T, hv *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	observer, err := hv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	// Histogram also implements Metric.
	h, ok := observer.(prometheus.Histogram)
	if !ok {
		t.Fatal("observer is not a Histogram")
	}
	if err := h.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestRecordScriptExecution_CounterIncrements(t *testing.T) {
	t.Parallel()
	m := sharedMetrics()
	ctx := context.Background()

	before := getCounterValue(t, m.scriptExecutions, "eval", "success")

	m.RecordScriptExecution(ctx, ScriptExecutionMetricsAttrs{
		Action:     "eval",
		Status:     "success",
		DurationMs: 100,
		ResultSize: 50,
		CodeSize:   20,
	})

	after := getCounterValue(t, m.scriptExecutions, "eval", "success")
	if after != before+1 {
		t.Errorf("counter = %v, want %v", after, before+1)
	}
}

func TestRecordScriptExecution_DurationMsZeroSkipsHistogram(t *testing.T) {
	t.Parallel()
	m := sharedMetrics()
	ctx := context.Background()

	before := getHistogramCount(t, m.scriptExecutionDuration, "eval_zero")

	m.RecordScriptExecution(ctx, ScriptExecutionMetricsAttrs{
		Action:     "eval_zero",
		Status:     "success",
		DurationMs: 0, // zero — should NOT record
		ResultSize: 50,
		CodeSize:   20,
	})

	after := getHistogramCount(t, m.scriptExecutionDuration, "eval_zero")
	if after != before {
		t.Errorf("duration histogram count = %d, want %d (should skip when DurationMs <= 0)", after, before)
	}
}

func TestRecordScriptExecution_DurationMsNegativeSkipsHistogram(t *testing.T) {
	t.Parallel()
	m := sharedMetrics()
	ctx := context.Background()

	before := getHistogramCount(t, m.scriptExecutionDuration, "eval_neg")

	m.RecordScriptExecution(ctx, ScriptExecutionMetricsAttrs{
		Action:     "eval_neg",
		Status:     "success",
		DurationMs: -1, // negative — should NOT record
		ResultSize: 50,
		CodeSize:   20,
	})

	after := getHistogramCount(t, m.scriptExecutionDuration, "eval_neg")
	if after != before {
		t.Errorf("duration histogram count = %d, want %d (should skip when DurationMs <= 0)", after, before)
	}
}

func TestRecordScriptExecution_ResultSizeZeroSkipsHistogram(t *testing.T) {
	t.Parallel()
	m := sharedMetrics()
	ctx := context.Background()

	before := getHistogramCount(t, m.scriptResultSize, "eval_nors")

	m.RecordScriptExecution(ctx, ScriptExecutionMetricsAttrs{
		Action:     "eval_nors",
		Status:     "success",
		DurationMs: 100,
		ResultSize: 0, // zero — should NOT record
		CodeSize:   20,
	})

	after := getHistogramCount(t, m.scriptResultSize, "eval_nors")
	if after != before {
		t.Errorf("result size histogram count = %d, want %d (should skip when ResultSize <= 0)", after, before)
	}
}

func TestRecordScriptExecution_CodeSizeZeroSkipsHistogram(t *testing.T) {
	t.Parallel()
	m := sharedMetrics()
	ctx := context.Background()

	before := getHistogramCount(t, m.scriptCodeSize, "eval_nocs")

	m.RecordScriptExecution(ctx, ScriptExecutionMetricsAttrs{
		Action:     "eval_nocs",
		Status:     "success",
		DurationMs: 100,
		ResultSize: 50,
		CodeSize:   0, // zero — should NOT record
	})

	after := getHistogramCount(t, m.scriptCodeSize, "eval_nocs")
	if after != before {
		t.Errorf("code size histogram count = %d, want %d (should skip when CodeSize <= 0)", after, before)
	}
}

func TestRecordScriptExecution_AllPositiveRecordsAllHistograms(t *testing.T) {
	t.Parallel()
	m := sharedMetrics()
	ctx := context.Background()

	durBefore := getHistogramCount(t, m.scriptExecutionDuration, "save_pos")
	resBefore := getHistogramCount(t, m.scriptResultSize, "save_pos")
	codeBefore := getHistogramCount(t, m.scriptCodeSize, "save_pos")

	m.RecordScriptExecution(ctx, ScriptExecutionMetricsAttrs{
		Action:     "save_pos",
		Status:     "failed",
		DurationMs: 500,
		ResultSize: 1024,
		CodeSize:   256,
	})

	durAfter := getHistogramCount(t, m.scriptExecutionDuration, "save_pos")
	resAfter := getHistogramCount(t, m.scriptResultSize, "save_pos")
	codeAfter := getHistogramCount(t, m.scriptCodeSize, "save_pos")

	if durAfter != durBefore+1 {
		t.Errorf("duration histogram: got %d, want %d", durAfter, durBefore+1)
	}
	if resAfter != resBefore+1 {
		t.Errorf("result size histogram: got %d, want %d", resAfter, resBefore+1)
	}
	if codeAfter != codeBefore+1 {
		t.Errorf("code size histogram: got %d, want %d", codeAfter, codeBefore+1)
	}
}
