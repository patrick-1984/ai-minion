package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format selects how log records are rendered.
type Format string

const (
	// FormatAuto renders text when the output is a terminal and JSON
	// otherwise.
	FormatAuto Format = "auto"
	// FormatJSON renders one JSON object per record.
	FormatJSON Format = "json"
	// FormatText renders key=value text.
	FormatText Format = "text"
)

// Options configures [New]. The zero value logs info and above to stderr, in
// JSON unless stderr is a terminal.
type Options struct {
	// Level is "debug", "info", "warn" or "error" (case-insensitive); empty
	// means info.
	Level string
	// Format is "auto", "json" or "text"; empty means auto.
	Format Format
	// Output is where records go; nil means [os.Stderr].
	Output io.Writer
	// IsTerminal decides whether Output is a terminal when Format is auto;
	// nil uses [IsTerminal]. It exists so that tests can force either mode.
	IsTerminal func(io.Writer) bool
}

// ParseLevel converts a level name to a [slog.Level].
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level %q, expected debug, info, warn or error", name)
}

// ParseFormat validates a format name; an empty name is [FormatAuto].
func ParseFormat(name string) (Format, error) {
	switch f := Format(strings.ToLower(strings.TrimSpace(name))); f {
	case "", FormatAuto:
		return FormatAuto, nil
	case FormatJSON, FormatText:
		return f, nil
	}
	return "", fmt.Errorf("invalid log format %q, expected auto, json or text", name)
}

// New returns a logger that writes to opts.Output. Values of type [Secret]
// render as "[redacted]" in both formats.
func New(opts Options) (*slog.Logger, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	format, err := ParseFormat(string(opts.Format))
	if err != nil {
		return nil, err
	}
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}
	if format == FormatAuto {
		isTTY := opts.IsTerminal
		if isTTY == nil {
			isTTY = IsTerminal
		}
		format = FormatJSON
		if isTTY(out) {
			format = FormatText
		}
	}
	ho := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if format == FormatText {
		h = slog.NewTextHandler(out, ho)
	} else {
		h = slog.NewJSONHandler(out, ho)
	}
	return slog.New(h), nil
}

// IsTerminal reports whether w is a character device such as a terminal.
// Pipes, regular files and in-memory writers are not.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null (and NUL on Windows) is a character device but not a
	// terminal.
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, null) {
		return false
	}
	return true
}
