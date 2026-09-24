// Package logging configures slog for every MLDojo binary.
package logging

import (
	"io"
	"log/slog"
	"os"
)

// Setup installs the default logger.
//
//	MLDOJO_LOG_LEVEL=debug   more detail (default info)
//	MLDOJO_LOG_FORMAT=json   one JSON object per line, for Loki/ES
//
// Text stays the default: it is what a human tailing journalctl wants.
func Setup(w io.Writer) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if os.Getenv("MLDOJO_LOG_LEVEL") == "debug" {
		opts.Level = slog.LevelDebug
	}
	var h slog.Handler = slog.NewTextHandler(w, opts)
	if os.Getenv("MLDOJO_LOG_FORMAT") == "json" {
		h = slog.NewJSONHandler(w, opts)
	}
	slog.SetDefault(slog.New(h))
}
