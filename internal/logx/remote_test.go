package logx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Secrets in every form a record can carry them. The values are fakes in the
// exact formats of Minion's secrets.
const (
	fakeAPIKey    = "mn_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"
	fakeInvite    = "mnk_aaaabbbbccccddddeeeeffffgggghhhhiiiijjjjkkkkllllmmmm.zyxwvutsrqponmlkjihgfedcba765432zyxwvutsrqponmlkji"
	fakeInviteSec = "zyxwvutsrqponmlkjihgfedcba765432"
	fakeJoin      = "mnj1-eyJodWIiOiJodHRwczovL2h1Yi5leGFtcGxlIiwia2V5IjoibW5rX2FiYyJ9"
	fakeJoinBody  = "eyJodWIiOiJodHRwczovL2h1Yi5leGFtcGxl"
)

type stringer struct{ s string }

func (s stringer) String() string { return s.s }

type creds struct {
	User  string
	Token string // exported, so JSON renders it
	URL   string
}

func remoteCapture(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(RemoteSafe(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))), &buf
}

func TestRemoteSafeMasksEveryForm(t *testing.T) {
	cases := []struct {
		name string
		log  func(*slog.Logger)
		leak []string
	}{
		{"api key in message", func(l *slog.Logger) { l.Info("key " + fakeAPIKey + " used") }, []string{fakeAPIKey[3:]}},
		{"invite key in attr", func(l *slog.Logger) { l.Info("x", "note", "use "+fakeInvite) }, []string{fakeInviteSec}},
		{"join token in error", func(l *slog.Logger) { l.Info("x", "error", errors.New("bad token "+fakeJoin)) }, []string{fakeJoinBody}},
		{"pairing code canonical", func(l *slog.Logger) { l.Info("code MN-K7Q4-XY12") }, []string{"K7Q4", "XY12"}},
		{"pairing code dash-less", func(l *slog.Logger) { l.Info("x", "text", "code MNK7Q4XY12 now") }, []string{"K7Q4"}},
		{"pairing code lower case", func(l *slog.Logger) { l.Info("x", "text", "code mn-k7q4-xy12") }, []string{"k7q4"}},
		{"secret key", func(l *slog.Logger) { l.Info("x", "password", "pw-1", "api_key", "k-2") }, []string{"pw-1", "k-2"}},
		{"url userinfo and query", func(l *slog.Logger) { l.Info("x", "u", "https://u:p4ss@h/x?tok=q9") }, []string{"p4ss", "q9"}},
		{"bound attrs", func(l *slog.Logger) { l.With("token", "b0und", "u", "http://bu:bp@h").Info("x") }, []string{"b0und", "bp"}},
		{"nested group", func(l *slog.Logger) {
			l.Info("x", slog.Group("g", slog.Group("h", "password", "n3st", "v", fakeAPIKey)))
		}, []string{"n3st", fakeAPIKey[3:]}},
		{"group of a WithGroup logger", func(l *slog.Logger) { l.WithGroup("g").Info("x", "cookie", "c00k") }, []string{"c00k"}},
		{"error with url", func(l *slog.Logger) {
			l.Info("x", "cause", &url.Error{Op: "Get", URL: "https://eu:ep@h/p?s=1", Err: errors.New("refused")})
		}, []string{"ep@", "s=1"}},
		{"stringer", func(l *slog.Logger) { l.Info("x", "s", stringer{"see " + fakeAPIKey}) }, []string{fakeAPIKey[3:]}},
		{"struct", func(l *slog.Logger) {
			l.Info("x", "c", creds{User: "u", Token: fakeAPIKey, URL: "https://su:sp@h"})
		}, []string{fakeAPIKey[3:], "sp@"}},
		{"secret type", func(l *slog.Logger) { l.Info("x", "v", Secret("s3cr3t")) }, []string{"s3cr3t"}},
		{"map", func(l *slog.Logger) { l.Info("x", "m", map[string]string{"a": fakeJoin}) }, []string{fakeJoinBody}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, buf := remoteCapture(t)
			c.log(l)
			if buf.Len() == 0 {
				t.Fatal("nothing was written")
			}
			for _, leak := range c.leak {
				if strings.Contains(buf.String(), leak) {
					t.Errorf("leaks %q: %s", leak, buf.String())
				}
			}
		})
	}
}

func TestRemoteSafeDropsLocalOnlyRecords(t *testing.T) {
	var local bytes.Buffer
	remote, rbuf := remoteCapture(t)
	log := slog.New(Tee(slog.NewTextHandler(&local, nil), remote.Handler()))
	HumanNotice(context.Background(), log, slog.LevelError, "initial password", "password", "pw-9")
	log.Info("ordinary")
	if !strings.Contains(local.String(), "password=pw-9") {
		t.Fatalf("the console lost the notice: %s", local.String())
	}
	if strings.Contains(rbuf.String(), "pw-9") || strings.Contains(rbuf.String(), "initial password") {
		t.Fatalf("the remote sink got the notice: %s", rbuf.String())
	}
	if !strings.Contains(rbuf.String(), "ordinary") {
		t.Fatalf("the remote sink lost an ordinary record: %s", rbuf.String())
	}
}

// A human notice renders on the console exactly as the same call without the
// mark: the mark is in the context, not an attribute.
func TestHumanNoticeLocalOutputUnchanged(t *testing.T) {
	var a, b bytes.Buffer
	ho := &slog.HandlerOptions{ReplaceAttr: func(_ []string, at slog.Attr) slog.Attr {
		if at.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return at
	}}
	slog.New(slog.NewTextHandler(&a, ho)).Log(context.Background(), slog.LevelInfo, "pairing code", "pairing_code", "MN-AAAA-BBBB")
	HumanNotice(context.Background(), slog.New(slog.NewTextHandler(&b, ho)), slog.LevelInfo, "pairing code", "pairing_code", "MN-AAAA-BBBB")
	if a.String() != b.String() {
		t.Fatalf("local output differs:\n%s\n%s", a.String(), b.String())
	}
}

func TestRemoteSafeCapsSizes(t *testing.T) {
	l, buf := remoteCapture(t)
	big := strings.Repeat("\u00e9", 10000) // 20000 bytes
	l.Info(big, "a", big)
	out := buf.String()
	if !utf8.ValidString(out) {
		t.Fatal("output is not valid UTF-8")
	}
	if !strings.Contains(out, "[truncated ") {
		t.Fatalf("no truncation marker: %.200s", out)
	}
	if len(out) > RemoteMaxRecord+1024 {
		t.Fatalf("record of %d bytes exceeds the cap", len(out))
	}
	buf.Reset()
	args := []any{}
	for i := range 20 {
		args = append(args, fmt.Sprintf("k%d", i), strings.Repeat("x", 1500))
	}
	l.Info("many", args...)
	if !strings.Contains(buf.String(), `"attrs_truncated":`) || buf.Len() > RemoteMaxRecord+1024 {
		t.Fatalf("attributes over the record cap were not cut (%d bytes)", buf.Len())
	}
}

func TestCapTextRuneBoundary(t *testing.T) {
	for _, max := range []int{30, 31, 32, 33, 40} {
		got := CapText(strings.Repeat("€", 30), max)
		if !utf8.ValidString(got) || len(got) > max {
			t.Errorf("CapText(%d) = %q (%d bytes)", max, got, len(got))
		}
	}
	if CapText("short", 10) != "short" {
		t.Error("short text changed")
	}
}

func TestRedactTextMinionTokens(t *testing.T) {
	got := RedactText("a " + fakeAPIKey + " b " + fakeInvite + " c " + fakeJoin)
	for _, leak := range []string{fakeAPIKey[3:], fakeInviteSec, fakeJoinBody} {
		if strings.Contains(got, leak) {
			t.Errorf("leaks %q: %s", leak, got)
		}
	}
	for _, keep := range []string{"mn_[redacted]", "mnk_aaaabbbbccccddddeeeeffffgggghhhhiiiijjjjkkkkllllmmmm.[redacted]", "mnj1-[redacted]"} {
		if !strings.Contains(got, keep) {
			t.Errorf("lacks %q: %s", keep, got)
		}
	}
}

type innerCreds struct {
	Authorization string `json:"authorization"`
	Nested        struct {
		APIKey string
		Note   string
	}
}

type secretBytes struct {
	Fingerprint string
	Secret      []byte
}

// Secret members of compound values are masked at any depth, also when the
// outer key is harmless and the value has no known token format.
func TestRemoteSafeMasksNestedSecrets(t *testing.T) {
	l, buf := remoteCapture(t)
	var c innerCreds
	c.Authorization = "Bearer opaque-cred-1"
	c.Nested.APIKey = "opaque-cred-2"
	c.Nested.Note = "kept"
	l.Info("x", "req", c, "key", "x", "invite", secretBytes{Fingerprint: "fpr", Secret: []byte("opaque-cred-3")},
		"list", []map[string]string{{"password": "opaque-cred-4"}})
	for _, leak := range []string{"opaque-cred-1", "opaque-cred-2", "b3BhcXVlLWNyZWQtMw", "opaque-cred-4"} {
		if strings.Contains(buf.String(), leak) {
			t.Fatalf("leaks %q: %s", leak, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "kept") || !strings.Contains(buf.String(), "fpr") {
		t.Fatalf("lost ordinary members: %s", buf.String())
	}
}

// An apostrophe is legal in a URL's user information and query.
func TestRedactURLWithApostrophes(t *testing.T) {
	got := RedactText("dial https://us'er:p'ass@h.example/x?a=1'2&token=t0k now")
	for _, leak := range []string{"p'ass", "t0k"} {
		if strings.Contains(got, leak) {
			t.Fatalf("leaks %q: %s", leak, got)
		}
	}
}

// Invalid UTF-8 is replaced in messages, keys, values and group names.
func TestRemoteSafeMakesUTF8Valid(t *testing.T) {
	l, buf := remoteCapture(t)
	l.WithGroup("g\xff").Info("m\xfe", "k\xff", "v\xfd", "e", errors.New("bad \xff"), "s", stringer{"s\xff"})
	if !utf8.Valid(buf.Bytes()) || strings.Contains(buf.String(), `�`) && !strings.Contains(buf.String(), "�") {
		t.Fatalf("invalid output: %q", buf.String())
	}
	h := RemoteSafe(slogCheck{t})
	_ = h.WithGroup("g\xff").Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "m\xfe", 0))
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "m\xfe", 0)
	r.AddAttrs(slog.String("k\xff", "v\xfd"), slog.Any("e", errors.New("bad \xff")), slog.Group("g\xfe", slog.String("a", "\xff")))
	_ = h.Handle(context.Background(), r)
}

// slogCheck fails the test on any invalid UTF-8 it is handed.
type slogCheck struct{ t *testing.T }

func (c slogCheck) Enabled(context.Context, slog.Level) bool { return true }
func (c slogCheck) Handle(_ context.Context, r slog.Record) error {
	if !utf8.ValidString(r.Message) {
		c.t.Errorf("message %q", r.Message)
	}
	var walk func(a slog.Attr)
	walk = func(a slog.Attr) {
		if !utf8.ValidString(a.Key) {
			c.t.Errorf("key %q", a.Key)
		}
		switch a.Value.Kind() {
		case slog.KindString:
			if !utf8.ValidString(a.Value.String()) {
				c.t.Errorf("value %q", a.Value.String())
			}
		case slog.KindGroup:
			for _, g := range a.Value.Group() {
				walk(g)
			}
		}
	}
	r.Attrs(func(a slog.Attr) bool { walk(a); return true })
	return nil
}
func (c slogCheck) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c slogCheck) WithGroup(n string) slog.Handler {
	if !utf8.ValidString(n) {
		c.t.Errorf("group %q", n)
	}
	return c
}

// A known token right after a newline inside a nested value is masked (in
// JSON text it would follow the "n" of the escape).
func TestRemoteSafeNestedTokenAfterNewline(t *testing.T) {
	l, buf := remoteCapture(t)
	l.Info("x", "req", map[string]any{"note": "first line\n" + fakeAPIKey, "list": []string{"a\t" + fakeJoin}})
	for _, leak := range []string{fakeAPIKey[3:], fakeJoinBody} {
		if strings.Contains(buf.String(), leak) {
			t.Fatalf("leaks %q: %s", leak, buf.String())
		}
	}
}
