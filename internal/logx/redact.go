package logx

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// SafeURL is a URL as a log line may show it: scheme and host only. User
// information (a proxy or mirror credential, also a username-only token),
// path, query and fragment are left out. Text that is no absolute URL comes
// back as "[url]"; "" stays "".
func SafeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[url]"
	}
	return u.Scheme + "://" + u.Host
}

var (
	// urlUserinfo is the user information of a URL inside free text.
	urlUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/?#@"'<>]+@`)
	// urlQuery is the query of a URL inside free text (it can carry a token).
	urlQuery = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s?#"'<>]*)\?[^\s#"'<>]*`)
	// pairingCode is a pairing code MN-XXXX-XXXX, also typed without dashes.
	pairingCode = regexp.MustCompile(`(?i)\bMN-?[0-9A-Z]{4}-?[0-9A-Z]{4}\b`)
)

// RedactText masks what free text (an error message, a log message) must not
// carry into a log that leaves the machine: the user information and the query
// of every URL in it, and pairing codes.
func RedactText(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "${1}"+Redacted+"@")
	s = urlQuery.ReplaceAllString(s, "${1}?"+Redacted)
	return pairingCode.ReplaceAllString(s, "MN-"+Redacted)
}

// secretKeys are attribute keys whose values never reach the in-memory log:
// an exact match, or a key that ends in "_" plus one of them.
var secretKeys = []string{
	"pairing_code", "code_plain", "token", "password", "passwd", "secret", "key",
	"authorization", "cookie", "set_cookie", "credential", "credentials", "private_key", "api_key", "apikey",
}

// secretKey reports whether an attribute key names a secret.
func secretKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range secretKeys {
		if k == s || strings.HasSuffix(k, "_"+s) || strings.HasSuffix(k, "-"+s) {
			return true
		}
	}
	return false
}

// redactAttr masks one attribute for the in-memory log (see [RedactText] and
// [secretKey]); groups are masked member by member.
func redactAttr(a slog.Attr) slog.Attr {
	if secretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactText(v.String()))
	case slog.KindGroup:
		gs := v.Group()
		out := make([]any, 0, len(gs))
		for _, g := range gs {
			out = append(out, redactAttr(g))
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, RedactText(err.Error()))
		}
		if s, ok := v.Any().(interface{ String() string }); ok {
			return slog.String(a.Key, RedactText(s.String()))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// redactHandler masks secrets before a record reaches the handler it wraps:
// the in-memory log, which the Hub can read (worker_logs), never holds a
// pairing code, a credential or a URL's user information, even where the local
// log shows one on purpose (the pairing code for the human, AD-20).
type redactHandler struct{ h slog.Handler }

func (r redactHandler) Enabled(ctx context.Context, l slog.Level) bool { return r.h.Enabled(ctx, l) }

func (r redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, RedactText(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return r.h.Handle(ctx, out)
}

func (r redactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = redactAttr(a)
	}
	return redactHandler{r.h.WithAttrs(out)}
}

func (r redactHandler) WithGroup(name string) slog.Handler { return redactHandler{r.h.WithGroup(name)} }
