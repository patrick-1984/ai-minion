package presence

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSanitizeHubOrigin(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"https://hub.example:8443":          "https://hub.example:8443",
		"https://HUB.example/proxy/tok?a=b": "https://hub.example",
		"wss://user:pw@hub.example/minion":  "https://hub.example",
		"ws://hub.example:80#frag":          "http://hub.example:80",
		"https://[::1]:9000/x":              "https://[::1]:9000",
		"ftp://hub.example":                 "",
		"https://hub.example:99999":         "",
		"not a url":                         "",
		"https://":                          "",
	}
	for in, want := range cases {
		got := SanitizeHubOrigin(in)
		if got != want {
			t.Errorf("SanitizeHubOrigin(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, "?#@") || strings.Count(got, "/") > 2 {
			t.Errorf("%q leaks a component: %q", in, got)
		}
	}
}

func TestDecodeRecordIsStrict(t *testing.T) {
	good := `{"schema":"minion.presence/1","instance":"abc-1","daemon_version":"v1","started_at":1,"updated_at":2,"stale_after_s":90,"state":"running","os_scope":"user"}`
	if _, err := DecodeRecord([]byte(good)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"unknown field": strings.Replace(good, `"user"}`, `"user","owner":"alice"}`, 1),
		"trailing":      good + `{}`,
		"state":         strings.Replace(good, `"running"`, `"starting"`, 1),
		"instance dots": strings.Replace(good, `abc-1`, `../x`, 1),
		"scope":         strings.Replace(good, `"user"`, `"system"`, 1),
		"origin path":   strings.Replace(good, `"user"}`, `"user","hub_origin":"https://h.example/p"}`, 1),
		"stale zero":    strings.Replace(good, `"stale_after_s":90`, `"stale_after_s":0`, 1),
		"not json":      `x`,
		// Decoder.More does not see these.
		"trailing bracket": good + `]`,
		"trailing brace":   good + `}`,
		"trailing comma":   good + `,`,
		"missing started":  strings.Replace(good, `"started_at":1,`, ``, 1),
		"null started":     strings.Replace(good, `"started_at":1`, `"started_at":null`, 1),
		"null pid":         strings.Replace(good, `"user"}`, `"user","pid":null}`, 1),
		"zero pid":         strings.Replace(good, `"user"}`, `"user","pid":0}`, 1),
		"bad link":         strings.Replace(good, `"user"}`, `"user","hub_link":"up"}`, 1),
		"empty link":       strings.Replace(good, `"user"}`, `"user","hub_link":""}`, 1),
		"not an object":    `[` + good + `]`,
	}
	for n, b := range bad {
		if _, err := DecodeRecord([]byte(b)); err == nil {
			t.Errorf("%s: accepted", n)
		}
	}
	for _, link := range []string{"connected", "registration_pending", "disconnected", "none"} {
		if r, err := DecodeRecord([]byte(strings.Replace(good, `"user"}`, `"user","hub_link":"`+link+`"}`, 1))); err != nil || r.HubLink != link {
			t.Errorf("%s: %v", link, err)
		}
	}
}

func TestRenameWithRetryIsBounded(t *testing.T) {
	oldRename, oldRetry, oldSleep := renameFile, renameRetryable, renameSleep
	defer func() { renameFile, renameRetryable, renameSleep = oldRename, oldRetry, oldSleep }()
	var slept time.Duration
	renameSleep = func(d time.Duration) { slept += d }
	renameRetryable = func(error) bool { return true }

	// Succeeds after the reader lets go.
	calls := 0
	renameFile = func(string, string) error {
		if calls++; calls < 5 {
			return errors.New("sharing violation")
		}
		return nil
	}
	if err := renameWithRetry("a", "b", nil); err != nil || calls != 5 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	// Gives up after a bound.
	calls = 0
	renameFile = func(string, string) error { calls++; return errors.New("sharing violation") }
	if err := renameWithRetry("a", "b", nil); err == nil || calls != renameAttempts {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if slept > 2*time.Second+5*time.Second { // sanity: well bounded
		t.Fatalf("slept %v", slept)
	}
	// A non-retryable error returns at once.
	calls = 0
	renameRetryable = func(error) bool { return false }
	if err := renameWithRetry("a", "b", nil); err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

// The authorization is asked before every attempt, not once before the loop: a
// condition that ends while a reader holds the destination stops the rename.
func TestRenameRetryRechecksTheGateBeforeEveryAttempt(t *testing.T) {
	oldRename, oldRetry, oldSleep := renameFile, renameRetryable, renameSleep
	defer func() { renameFile, renameRetryable, renameSleep = oldRename, oldRetry, oldSleep }()
	renameSleep = func(time.Duration) {}
	renameRetryable = func(error) bool { return true }
	renamed := 0
	open := true
	renameFile = func(string, string) error {
		if renamed++; renamed == 1 {
			open = false // the Hub link expires while the reader holds the file
			return errors.New("sharing violation")
		}
		return nil // the reader let go
	}
	err := renameWithRetry("a", "b", func() bool { return open })
	if !errors.Is(err, errGateClosed) || renamed != 1 {
		t.Fatalf("err=%v renames=%d, want the gate to stop the second attempt", err, renamed)
	}
}

// usageDoc is a minimal valid usage-meters document.
func usageDoc(generated int) string {
	return `{"schema":"minion.usage-meters/1","generated_at":` + strconv.Itoa(generated) + `,"slots":[]}`
}
