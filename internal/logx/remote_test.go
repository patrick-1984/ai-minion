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
