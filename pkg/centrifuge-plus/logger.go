package centrifugeplus

import (
	"log"
	"os"
)

// pkgLogger is a dedicated logger instance for this package. Using a private
// *log.Logger avoids mutating the global log.SetOutput, which would affect
// every component that uses the standard log package.
var pkgLogger = log.New(os.Stderr, "[centrifuge-plus] ", log.LstdFlags)

// Logger defines the logging interface used by AsynqBroker.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type defaultLogger struct{}

func (defaultLogger) Info(msg string, args ...any) {
	pkgLogger.Printf("INFO: "+msg, args...)
}

func (defaultLogger) Warn(msg string, args ...any) {
	pkgLogger.Printf("WARNING: "+msg, args...)
}

func (defaultLogger) Error(msg string, args ...any) {
	pkgLogger.Printf("ERROR: "+msg, args...)
}
