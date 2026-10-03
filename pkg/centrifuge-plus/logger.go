package centrifugeplus

import (
	"fmt"
)

// LogFunc is the signature for logging functions injected from the application.
type LogFunc func(msg string, fields ...any)

// logFuncs holds the injected logging functions.
// Defaults to no-op; call SetLogFuncs after logger.Init to enable logging.
var logFuncs = struct {
	Info  LogFunc
	Warn  LogFunc
	Error LogFunc
}{
	Info:  func(msg string, fields ...any) {},
	Warn:  func(msg string, fields ...any) {},
	Error: func(msg string, fields ...any) {},
}

// SetLogFuncs injects logging functions from the application's logger.
// Call this after logger.Init so that centrifuge-plus logs are routed
// through the application's smart caller resolution.
func SetLogFuncs(info, warn, err LogFunc) {
	if info != nil {
		logFuncs.Info = info
	}
	if warn != nil {
		logFuncs.Warn = warn
	}
	if err != nil {
		logFuncs.Error = err
	}
}

// Logger defines the logging interface used by AsynqBroker.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type defaultLogger struct{}

func (defaultLogger) Info(msg string, args ...any) {
	logFuncs.Info(fmt.Sprintf(msg, args...))
}

func (defaultLogger) Warn(msg string, args ...any) {
	logFuncs.Warn(fmt.Sprintf(msg, args...))
}

func (defaultLogger) Error(msg string, args ...any) {
	logFuncs.Error(fmt.Sprintf(msg, args...))
}
