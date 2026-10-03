// Package logger provides the application logger built on log/slog.
package logger

import (
	"log/slog"
	"os"
)

// Default is the application logger, writing structured logs to stdout.
var Default = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

// SetLevel changes the minimum level of the default logger.
func SetLevel(l slog.Level) {
	Default = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// Debug logs a message at DEBUG level.
func Debug(msg string, args ...any) { Default.Debug(msg, args...) }

// Info logs a message at INFO level.
func Info(msg string, args ...any) { Default.Info(msg, args...) }

// Warn logs a message at WARN level.
func Warn(msg string, args ...any) { Default.Warn(msg, args...) }

// Error logs a message at ERROR level.
func Error(msg string, args ...any) { Default.Error(msg, args...) }

// Fatal logs a message at ERROR level and exits with status 1.
func Fatal(msg string, args ...any) {
	Default.Error(msg, args...)
	os.Exit(1)
}
