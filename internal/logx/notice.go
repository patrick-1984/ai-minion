package logx

import (
	"context"
	"log/slog"
)

// LocalOnlyKey names the mark of a record that must stay on this machine: a
// remote sink ([RemoteSafe]) drops such a record whole. The mark travels in
// the context of the record, not as an attribute, so that the local output of
// a marked record is exactly what it was before the mark existed.
const LocalOnlyKey = "minion.local_only"

type localOnlyKey struct{}

// LocalOnly returns ctx marked so that a record logged with it never leaves
// the machine: the console (stderr) and the in-memory log keep it, every
// remote sink drops it.
func LocalOnly(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, localOnlyKey{}, true)
}

// IsLocalOnly reports whether ctx carries the mark of [LocalOnly].
func IsLocalOnly(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	on, _ := ctx.Value(localOnlyKey{}).(bool)
	return on
}

// HumanNotice logs a record that is meant for the person at the console and
// carries a working credential on purpose (the generated initial admin
// password, the pairing code, AD-20). The record keeps its level and text in
// the local log; remote sinks (syslog, OpenTelemetry) drop it whole.
func HumanNotice(ctx context.Context, log *slog.Logger, level slog.Level, msg string, args ...any) {
	if log == nil {
		return
	}
	log.Log(LocalOnly(ctx), level, msg, args...)
}
