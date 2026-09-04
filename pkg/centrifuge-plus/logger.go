package centrifugeplus

import "log"

// Logger defines the logging interface used by AsynqBroker.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type defaultLogger struct{}

func (defaultLogger) Info(msg string, args ...any) {
	log.Printf("info: "+msg, args...)
}

func (defaultLogger) Warn(msg string, args ...any) {
	log.Printf("warning: "+msg, args...)
}

func (defaultLogger) Error(msg string, args ...any) {
	log.Printf("error: "+msg, args...)
}
