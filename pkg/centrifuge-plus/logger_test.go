package centrifugeplus

import (
	"testing"
)

func TestDefaultLogger_Info(_ *testing.T) {
	l := defaultLogger{}
	// 不应 panic
	l.Info("test info: %s", "arg1")
}

func TestDefaultLogger_Warn(_ *testing.T) {
	l := defaultLogger{}
	l.Warn("test warn: %d", 42)
}

func TestDefaultLogger_Error(_ *testing.T) {
	l := defaultLogger{}
	l.Error("test error: %v", "something failed")
}

func TestDefaultLogger_MultipleArgs(_ *testing.T) {
	l := defaultLogger{}
	l.Info("multi args: %s %d %v", "str", 123, true)
}

func TestDefaultLogger_NoArgs(_ *testing.T) {
	l := defaultLogger{}
	l.Info("no args message")
	l.Warn("no args warning")
	l.Error("no args error")
}
