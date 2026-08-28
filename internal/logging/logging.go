// Package logging builds a slog logger for the level from the config.
package logging

import (
	"context"
	"io"
	"log/slog"
)

// levelNone disables output entirely (log_level: none).
const levelNone = slog.Level(1 << 20)

// Levels lists the allowed log_level values.
var Levels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
	"none":  levelNone,
}

// New returns a logger for the given level and writer w.
func New(level string, w io.Writer) *slog.Logger {
	l, ok := Levels[level]
	if !ok {
		l = slog.LevelInfo
	}
	if l == levelNone {
		return slog.New(discardHandler{})
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
