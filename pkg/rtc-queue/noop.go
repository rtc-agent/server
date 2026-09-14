package rtcqueue

// noopWorkerLogger implements WorkerLogger with no-op operations.
// Used as a default when no logger is provided, eliminating nil checks
// at every call site (the logIfEnabled anti-pattern).
type noopWorkerLogger struct{}

func (noopWorkerLogger) Info(_ string, _ ...any)  {}
func (noopWorkerLogger) Error(_ string, _ ...any) {}

// Compile-time assertion that noopWorkerLogger implements WorkerLogger.
var _ WorkerLogger = noopWorkerLogger{}
