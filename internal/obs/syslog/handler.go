package syslog

import (
	"context"
	"log/slog"

	"github.com/patrick-1984/ai-minion/internal/logx"
)

// MsgIDKey is the attribute whose value becomes the MSGID header field.
const MsgIDKey = "component"

// NewHandler returns the slog handler of a syslog sink: logx.RemoteSafe in
// front of a handler that formats each record with f and queues it on w. It
// never blocks the caller. Levels are filtered by the slot of the
// logx.Fanout the handler is put into.
func NewHandler(f *Formatter, w *Writer) slog.Handler {
	return logx.RemoteSafe(&handler{f: f, w: w})
}

type groupFrame struct {
	name   string
	fields []Field
}

type handler struct {
	f      *Formatter
	w      *Writer
	bound  []Field // top-level fields bound before any group
	groups []groupFrame
	msgID  string
}

func (h *handler) Enabled(context.Context, slog.Level) bool { return true }

func (h *handler) Handle(_ context.Context, rec slog.Record) error {
	var as []slog.Attr
	msgID := h.msgID
	rec.Attrs(func(a slog.Attr) bool {
		as = append(as, a)
		if len(h.groups) == 0 && a.Key == MsgIDKey && a.Value.Kind() == slog.KindString {
			msgID = a.Value.String()
		}
		return true
	})
	inner := Fields(as)
	for i := len(h.groups) - 1; i >= 0; i-- {
		g := h.groups[i]
		fs := append(append([]Field(nil), g.fields...), inner...)
		if len(fs) == 0 {
			inner = nil
			continue
		}
		inner = []Field{{Key: g.name, Value: ObjectJSON(fs)}}
	}
	all := append(append([]Field(nil), h.bound...), inner...)
	h.w.Enqueue(h.f.Format(rec.Time, rec.Level, msgID, rec.Message, all))
	return nil
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	out := *h
	fs := Fields(as)
	if len(h.groups) == 0 {
		out.bound = append(append([]Field(nil), h.bound...), fs...)
		for _, a := range as {
			if a.Key == MsgIDKey && a.Value.Kind() == slog.KindString {
				out.msgID = a.Value.String()
			}
		}
		return &out
	}
	out.groups = append([]groupFrame(nil), h.groups...)
	last := &out.groups[len(out.groups)-1]
	last.fields = append(append([]Field(nil), last.fields...), fs...)
	return &out
}

func (h *handler) WithGroup(name string) slog.Handler {
	out := *h
	out.groups = append(append([]groupFrame(nil), h.groups...), groupFrame{name: name})
	return &out
}
