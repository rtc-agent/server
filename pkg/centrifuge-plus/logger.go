package centrifugeplus

import (
	"log"
	"os"
)

func init() {
	// Direct the default log package to stderr so that defaultLogger output
	// does not interleave with stdout-oriented tool output.
	log.SetOutput(os.Stderr)
}

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
