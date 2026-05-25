// Package log is the Bellerophon structured-logging wrapper around log/slog.
// Implementations satisfy the Logger interface so call sites can be
// retargeted (e.g. to a test sink) without changing imports.
package log

import (
	"log/slog"
	"os"
	"strings"
)

// Logger is the minimal structured-logging surface used by Bellerophon.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	With(args ...any) Logger
}

type slogLogger struct{ l *slog.Logger }

func (s *slogLogger) Debug(msg string, args ...any) { s.l.Debug(msg, args...) }
func (s *slogLogger) Info(msg string, args ...any)  { s.l.Info(msg, args...) }
func (s *slogLogger) Warn(msg string, args ...any)  { s.l.Warn(msg, args...) }
func (s *slogLogger) Error(msg string, args ...any) { s.l.Error(msg, args...) }
func (s *slogLogger) With(args ...any) Logger       { return &slogLogger{l: s.l.With(args...)} }

// New returns a Logger writing structured text to stderr at the given level.
// Accepted level strings: debug, info, warn, error. Unknown values default to info.
func New(level string) Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return &slogLogger{l: slog.New(h)}
}
