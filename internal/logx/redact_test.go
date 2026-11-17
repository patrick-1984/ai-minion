package logx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestSafeURLKeepsSchemeAndHostOnly(t *testing.T) {
	for in, want := range map[string]string{
		"http://tok123@proxy.example:8080":           "http://proxy.example:8080",
		"https://user:pw@mirror.example/r?token=abc": "https://mirror.example",
		"wss://hub.example/api/v1/worker/connect":    "wss://hub.example",
		"":          "",
		"not a url": "[url]",
	} {
		if got := SafeURL(in); got != want {
			t.Errorf("SafeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactTextMasksCredentialsAndCodes(t *testing.T) {
	in := `Get "https://tok123@mirror.example/rel?sig=s3cr3t": proxyconnect http://user:pw@proxy:3128 failed; code MN-K7Q4-XY12 or mnk7q4xy12`
	got := RedactText(in)
	for _, leak := range []string{"tok123", "s3cr3t", "user:pw", "K7Q4", "k7q4"} {
		if strings.Contains(got, leak) {
			t.Errorf("RedactText leaks %q: %s", leak, got)
		}
	}
	for _, keep := range []string{"https://[redacted]@mirror.example/rel?[redacted]", "http://[redacted]@proxy:3128", "MN-[redacted]"} {
		if !strings.Contains(got, keep) {
			t.Errorf("RedactText lacks %q: %s", keep, got)
		}
	}
}

// The Hub can read the ring, so it never holds a pairing code or another
// secret, while the local output keeps what it shows on purpose.
func TestRingNeverHoldsSecrets(t *testing.T) {
	var local bytes.Buffer
	ring := NewRing(100, 1<<20)
	log, _, err := NewLive(Options{Level: "debug", Format: FormatText, Output: &local}, ring)
	if err != nil {
		t.Fatal(err)
	}
	log = log.With("api_token", "tok-with")
	log.Info("pairing code MN-ABCD-EFGH", "pairing_code", "MN-ABCD-EFGH", "Authorization", "Bearer x1",
		"cookie", "sid=c00k", "proxy", "http://ptok@proxy:1", "err", errors.New("GET https://u:p4ss@m/x failed"),
		slog.Group("g", "password", "pw1"))
	if !strings.Contains(local.String(), "MN-ABCD-EFGH") {
		t.Fatalf("the local log lost the pairing code: %s", local.String())
	}
	got := strings.Join(ring.Tail(0), "\n")
	for _, leak := range []string{"ABCD", "tok-with", "x1", "c00k", "ptok", "p4ss", "pw1"} {
		if strings.Contains(got, leak) {
			t.Errorf("the ring holds %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "pairing_code=[redacted]") {
		t.Errorf("ring = %s", got)
	}
}

func TestRingCapsLinesToItsBudgetAndResets(t *testing.T) {
	r := NewRing(10, 10)
	r.Add(strings.Repeat("x", 20))
	if _, b, _ := r.Stats(); b > 10 {
		t.Fatalf("bytes = %d, budget 10", b)
	}
	r.Add("ok")
	r.Reset()
	if n, b, dropped := r.Stats(); n != 0 || b != 0 || dropped < 1 || len(r.Tail(0)) != 0 {
		t.Fatalf("after Reset: %d lines %d bytes %d dropped", n, b, dropped)
	}
}

// An unescaped '@' in the user name or password: the user information runs
// to the LAST '@' of the authority, so nothing of the password stays.
func TestRedactTextMasksUserinfoUpToTheLastAt(t *testing.T) {
	got := RedactText(`proxyconnect https://us@er:p@ss@host/x failed`)
	for _, leak := range []string{"us@", "er:", "p@ss", "ss@"} {
		if strings.Contains(got, leak) {
			t.Errorf("RedactText leaks %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "https://[redacted]@host/x") {
		t.Errorf("RedactText = %s", got)
	}
	if got := SafeURL("https://us@er:p@ss@host/x"); got != "https://host" && got != "[url]" {
		t.Errorf("SafeURL = %q", got)
	}
}

// Errors keep scheme and host of every URL: no user information, path,
// query or fragment.
func TestRedactErrorTextCutsURLsToSchemeAndHost(t *testing.T) {
	in := `dial: GET "https://us@er:p@ss@host:8443/tok3n/prefix?sig=s1#frag": refused; proxy http://u:pw@proxy:3128/x`
	got := RedactErrorText(in)
	for _, leak := range []string{"us@", "p@ss", "tok3n", "prefix", "s1", "frag", "u:pw", "/x"} {
		if strings.Contains(got, leak) {
			t.Errorf("RedactErrorText leaks %q: %s", leak, got)
		}
	}
	for _, keep := range []string{`"https://host:8443"`, "http://proxy:3128"} {
		if !strings.Contains(got, keep) {
			t.Errorf("RedactErrorText lacks %q: %s", keep, got)
		}
	}
}

// A *url.Error (what net/http returns) is rebuilt with SafeURL, also when it
// is wrapped.
func TestErrorTextRebuildsURLErrors(t *testing.T) {
	for _, raw := range []string{"https://us@er:p@ss@host/x", "https://hub.example/s3cr3t-prefix/api?k=v", "https://tok@hub.example/p"} {
		ue := &url.Error{Op: "Get", URL: raw, Err: errors.New("connection refused")}
		got := ErrorText(fmt.Errorf("dial the hub: %w", ue))
		for _, leak := range []string{"us@", "p@ss", "s3cr3t", "k=v", "tok@", "/p", "/x"} {
			if strings.Contains(got, leak) {
				t.Errorf("ErrorText(%q) leaks %q: %s", raw, leak, got)
			}
		}
		if !strings.Contains(got, "dial the hub: Get ") || !strings.Contains(got, "connection refused") {
			t.Errorf("ErrorText(%q) = %s", raw, got)
		}
	}
	if ErrorText(nil) != "" {
		t.Error("ErrorText(nil) is not empty")
	}
}

// The ring cuts URLs in error attributes, given as an error or as text.
func TestRingCutsURLsInErrors(t *testing.T) {
	ring := NewRing(100, 1<<20)
	log, _, err := NewLive(Options{Level: "debug", Format: FormatText, Output: io.Discard}, ring)
	if err != nil {
		t.Fatal(err)
	}
	log.Warn("update failed", "error", "GET https://us@er:p@ss@mirror/tok3n/r: 404",
		"err", &url.Error{Op: "Get", URL: "https://hub/s3cr3t", Err: errors.New("EOF")})
	got := strings.Join(ring.Tail(0), "\n")
	for _, leak := range []string{"p@ss", "tok3n", "s3cr3t"} {
		if strings.Contains(got, leak) {
			t.Errorf("the ring holds %q: %s", leak, got)
		}
	}
}
