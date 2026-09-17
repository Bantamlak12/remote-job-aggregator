// Package observability builds the process-wide structured logger.
package observability

import (
	"io"
	"log/slog"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
)

// NewLogger builds a slog.Logger per the given LogConfig, writing to w.
func NewLogger(cfg config.LogConfig, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Level}

	var handler slog.Handler
	switch cfg.Format {
	case config.LogFormatText:
		handler = slog.NewTextHandler(w, opts)
	default:
		handler = slog.NewJSONHandler(w, opts)
	}

	return slog.New(handler)
}
