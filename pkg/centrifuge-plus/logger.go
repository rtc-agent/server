package centrifugeplus

import (
	"fmt"

	"go.uber.org/zap"
)

// pkgLogger is a dedicated logger instance for this package.
var pkgLogger = zap.NewNop().Named("centrifuge-plus")

// SetLogger replaces the package-level logger. Call this after logger.Init
// to route centrifuge-plus logs through the application's zap logger.
func SetLogger(l *zap.Logger) {
	if l != nil {
		pkgLogger = l.Named("centrifuge-plus")
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
	pkgLogger.Info(fmt.Sprintf(msg, args...))
}

func (defaultLogger) Warn(msg string, args ...any) {
	pkgLogger.Warn(fmt.Sprintf(msg, args...))
}

func (defaultLogger) Error(msg string, args ...any) {
	pkgLogger.Error(fmt.Sprintf(msg, args...))
}
