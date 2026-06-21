// Package logger provides structured logging utilities wrapping slog.
package logger

import (
	"io"
	"log/slog"
	"os"
)

// Logger wraps slog.Logger with convenience methods
type Logger struct {
	*slog.Logger
}

// New creates a new logger based on environment
func New(environment string) *Logger {
	var handler slog.Handler

	if environment == "production" {
		// JSON output for production
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})
	} else {
		// Pretty text output for development
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})
	}

	return &Logger{
		Logger: slog.New(handler),
	}
}

// NewWithWriter constructs a Logger whose output is routed to the given
// io.Writer. Used by tests to capture log output into a bytes.Buffer for
// assertion. Mirrors New("development") — pretty text handler at
// slog.LevelDebug — because that is the environment tests run in.
func NewWithWriter(w io.Writer) *Logger {
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	return &Logger{Logger: slog.New(handler)}
}

// With returns a new logger with additional context
func (l *Logger) With(args ...any) *Logger {
	return &Logger{
		Logger: l.Logger.With(args...),
	}
}

// Fatal logs a fatal error and exits
func (l *Logger) Fatal(msg string, args ...any) {
	l.Error(msg, args...)
	os.Exit(1)
}
