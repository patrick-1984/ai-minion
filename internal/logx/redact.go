package logx

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
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
	// urlUserinfo is the user information of a URL inside free text: the
	// authority up to its LAST '@', so that an unescaped '@' in a user name or
	// password (https://us@er:p@ss@host) is masked whole.
	urlUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/?#"<>]*@`)
	// urlWhole is a URL inside free text: scheme, authority and the rest up to
	// whitespace, a double quote or an angle bracket. An apostrophe is
	// legal inside a URL (user information, query), so it does not end one.
	urlWhole = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^\s/?#"<>]*)[^\s"<>]*`)
	// urlQuery is the query of a URL inside free text (it can carry a token).
	urlQuery = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s?#"<>]*)\?[^\s#"<>]*`)
	// pairingCode is a pairing code MN-XXXX-XXXX, also typed without dashes.
	pairingCode = regexp.MustCompile(`(?i)\bMN-?[0-9A-Z]{4}-?[0-9A-Z]{4}\b`)
	// apiKeyText is a Minion API key: "mn_" plus 52 base32 characters
	// (internal/identity/apikey.go).
	apiKeyText = regexp.MustCompile(`(?i)\bmn_[a-z2-7]{52}`)
	// inviteKeyText is an invitation key mnk_<fingerprint>.<secret>
	// (internal/identity/invite.go); the fingerprint is public and kept.
	inviteKeyText = regexp.MustCompile(`(?i)\b(mnk_[a-z2-7]+)\.[a-z2-7]+`)
	// joinTokenText is a join token mnj1-<base64url payload>
	// (internal/identity/jointoken.go).
	joinTokenText = regexp.MustCompile(`(?i)\bmnj1-[A-Za-z0-9._~-]+`)
)

// RedactText masks what free text (an error message, a log message) must not
// carry into a log that leaves the machine: the user information and the query
// of every URL in it, Minion's own secret formats (API keys mn_..., invitation
// keys mnk_<fingerprint>.<secret>, join tokens mnj1-...) and pairing codes.
func RedactText(s string) string {
	// Cheap checks first: every pattern below needs "mn" (any case) or
	// "://", and most log text has neither.
	mn := hasMN(s)
	if mn {
		s = joinTokenText.ReplaceAllString(s, "mnj1-"+Redacted)
		s = inviteKeyText.ReplaceAllString(s, "${1}."+Redacted)
		s = apiKeyText.ReplaceAllString(s, "mn_"+Redacted)
	}
	if strings.Contains(s, "://") {
		s = urlUserinfo.ReplaceAllString(s, "${1}"+Redacted+"@")
		s = urlQuery.ReplaceAllString(s, "${1}?"+Redacted)
	}
	if mn {
		s = pairingCode.ReplaceAllString(s, "MN-"+Redacted)
	}
	return s
}

// hasMN reports whether s contains "mn" in any letter case.
func hasMN(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i]|0x20 == 'm' && s[i+1]|0x20 == 'n' {
			return true
		}
	}
	return false
}

// RedactErrorText masks free text of an error: every URL in it is cut to its
// scheme and host (no user information, path, query or fragment, which a
// reverse-proxy prefix or a mirror can use for a credential), then
// [RedactText] applies.
func RedactErrorText(s string) string {
	if !strings.Contains(s, "://") {
		return RedactText(s)
	}
	s = urlWhole.ReplaceAllStringFunc(s, func(m string) string {
		sub := urlWhole.FindStringSubmatch(m)
		host := sub[2]
		if i := strings.LastIndexByte(host, '@'); i >= 0 {
			host = host[i+1:]
		}
		return sub[1] + host
	})
	return RedactText(s)
}

// ErrorText is err's text for a log line, a remote log or a reported state:
// a *url.Error in its chain is rebuilt with [SafeURL] (an HTTP error quotes
// the whole request URL), and every other URL is cut by [RedactErrorText].
// nil is "".
func ErrorText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		safe := SafeURL(ue.URL)
		s = strings.ReplaceAll(s, strconv.Quote(ue.URL), strconv.Quote(safe))
		s = strings.ReplaceAll(s, ue.URL, safe)
	}
	return RedactErrorText(s)
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

// errorKey reports whether an attribute key holds an error text, whose URLs
// are cut to scheme and host ([RedactErrorText]).
func errorKey(k string) bool {
	switch strings.ToLower(k) {
	case "error", "err", "reason", "cause":
		return true
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
		if errorKey(a.Key) {
			return slog.String(a.Key, RedactErrorText(v.String()))
		}
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
			return slog.String(a.Key, ErrorText(err))
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
