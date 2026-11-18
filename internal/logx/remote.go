package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Size caps of a record that leaves the machine ([RemoteSafe]).
const (
	// RemoteMaxMessage caps the message of a remote record, in bytes.
	RemoteMaxMessage = 4 << 10
	// RemoteMaxString caps one string attribute of a remote record, in bytes.
	RemoteMaxString = 2 << 10
	// RemoteMaxRecord caps the message and all attributes of a remote record
	// together, in bytes (an estimate of the text, not of any wire format).
	RemoteMaxRecord = 16 << 10
)

// RemoteSafe wraps the handler of a remote sink (syslog, OpenTelemetry). It is
// the only way a remote handler is built, so that no remote sink ever sees a
// raw record:
//
//  1. a record logged with [LocalOnly] (a [HumanNotice]) is dropped whole;
//  2. the message and every attribute, the bound ones (WithAttrs) and the
//     members of groups included, are masked as for the in-memory log
//     (secret attribute keys, URL user information and queries, pairing
//     codes, Minion's API keys, invitation keys and join tokens);
//  3. a value of kind Any becomes text first: an error through [ErrorText], a
//     fmt.Stringer through [RedactText], any other value as JSON text that is
//     then masked, so that no struct escapes the text scan;
//  4. sizes are capped: message [RemoteMaxMessage], one string
//     [RemoteMaxString], the whole record [RemoteMaxRecord]; a cut ends on a
//     rune boundary with "…[truncated N bytes]".
func RemoteSafe(h slog.Handler) slog.Handler {
	if h == nil {
		return nil
	}
	if r, ok := h.(remoteHandler); ok {
		return r
	}
	return remoteHandler{h: h}
}

type remoteHandler struct {
	h slog.Handler
	// bound is the size of the attributes bound with WithAttrs, counted
	// against RemoteMaxRecord.
	bound int
}

func (r remoteHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return r.h.Enabled(ctx, l)
}

func (r remoteHandler) Handle(ctx context.Context, rec slog.Record) error {
	if IsLocalOnly(ctx) {
		return nil
	}
	msg := CapText(RedactText(validUTF8(rec.Message)), RemoteMaxMessage)
	out := slog.NewRecord(rec.Time, rec.Level, msg, rec.PC)
	budget := RemoteMaxRecord - len(msg) - r.bound
	cut := 0
	rec.Attrs(func(a slog.Attr) bool {
		ra := remoteAttr(a)
		n := attrSize(ra)
		if n > budget {
			cut++
			return true
		}
		budget -= n
		out.AddAttrs(ra)
		return true
	})
	if cut > 0 {
		out.AddAttrs(slog.Int("attrs_truncated", cut))
	}
	return r.h.Handle(ctx, out)
}

func (r remoteHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(as))
	bound := r.bound
	for _, a := range as {
		ra := remoteAttr(a)
		n := attrSize(ra)
		if bound+n > RemoteMaxRecord/2 {
			continue // bound attributes may use at most half the record
		}
		bound += n
		out = append(out, ra)
	}
	return remoteHandler{h: r.h.WithAttrs(out), bound: bound}
}

func (r remoteHandler) WithGroup(name string) slog.Handler {
	return remoteHandler{h: r.h.WithGroup(validUTF8(name)), bound: r.bound}
}

// validUTF8 replaces invalid UTF-8 with U+FFFD: an OTLP exporter rejects a
// whole batch for one invalid string, and syslog MSG must be UTF-8.
func validUTF8(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

// remoteAttr masks and normalises one attribute for a remote sink.
func remoteAttr(a slog.Attr) slog.Attr {
	a.Key = validUTF8(a.Key)
	if secretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	v := a.Value.Resolve() // a LogValuer (Secret included) resolves here
	switch v.Kind() {
	case slog.KindString:
		s := validUTF8(v.String())
		if errorKey(a.Key) {
			s = RedactErrorText(s)
		} else {
			s = RedactText(s)
		}
		return slog.String(a.Key, CapText(s, RemoteMaxString))
	case slog.KindGroup:
		gs := v.Group()
		out := make([]any, 0, len(gs))
		for _, g := range gs {
			out = append(out, remoteAttr(g))
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		return slog.String(a.Key, CapText(anyText(v.Any()), RemoteMaxString))
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// anyText renders a value of kind Any as masked text. A compound value
// (struct, map, slice) is rendered as JSON, then every member whose name is
// a secret key is replaced (at any depth, so a nested authorization field or
// the secret bytes of a nested key are masked), then the text is scanned.
func anyText(x any) string {
	switch t := x.(type) {
	case nil:
		return "<nil>"
	case error:
		return validUTF8(ErrorText(t))
	case fmt.Stringer:
		return RedactText(validUTF8(t.String()))
	}
	b, err := json.Marshal(x)
	if err != nil {
		return "[unrenderable " + fmt.Sprintf("%T", x) + "]"
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return "[unrenderable " + fmt.Sprintf("%T", x) + "]"
	}
	b, err = json.Marshal(maskTree(tree))
	if err != nil {
		return "[unrenderable " + fmt.Sprintf("%T", x) + "]"
	}
	return RedactText(string(b))
}

// maskTree replaces the values of secret keys in a decoded JSON tree.
func maskTree(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if secretKey(k) {
				t[k] = Redacted
				continue
			}
			t[k] = maskTree(x)
		}
	case []any:
		for i, x := range t {
			t[i] = maskTree(x)
		}
	}
	return v
}

// attrSize estimates the text size of an attribute.
func attrSize(a slog.Attr) int {
	n := len(a.Key) + 2
	switch a.Value.Kind() {
	case slog.KindString:
		return n + len(a.Value.String())
	case slog.KindGroup:
		for _, g := range a.Value.Group() {
			n += attrSize(g)
		}
		return n
	}
	return n + 24
}

// CapText returns s cut to at most max bytes, the marker included, on a rune
// boundary, with "…[truncated N bytes]" at the end when it was cut.
func CapText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	marker := func(n int) string { return "…[truncated " + strconv.Itoa(n) + " bytes]" }
	keep := max - len(marker(len(s)))
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + marker(len(s)-keep)
}
