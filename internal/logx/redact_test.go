package logx

import (
	"bytes"
	"errors"
	"log/slog"
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
