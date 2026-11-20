package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// stdinJoin runs "worker up --join -" on a host without a terminal (the way
// the tray does) with in as standard input.
func stdinJoin(t *testing.T, f *upFixture, in io.Reader, args ...string) (string, error) {
	t.Helper()
	return f.runIn(in, append([]string{"--join", "-"}, args...)...)
}

func shareOf(t *testing.T, dir string) store.ShareState {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s, err := st.LoadShareStateContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The token comes from standard input (first line only), the enrollment hook
// gets the Hub and the invitation key from it, a new profile shares nothing,
// and neither the token nor the inner key is in an argument, an output, a log
// or the config file.
func TestWorkerUpJoinFromStdinReadsOnlyTheToken(t *testing.T) {
	f, _ := serviceGate(t, false, false, "build")
	var seen joinSeen
	joinHook(t, f, &seen)
	text, tok := testJoinToken(t, "https://hub.example:8443")
	rest := "this line is never read\n"
	in := strings.NewReader(text + "\n" + rest)
	out, err := stdinJoin(t, f, in, "--share-existing", "none")
	if err != nil {
		t.Fatalf("worker up: %v\nstderr: %s", err, f.stderr)
	}
	if seen.hub.Server != "https://hub.example:8443" || seen.hub.Fingerprint != testHubFingerprint() || seen.invite.Value != tok.Invite.String() {
		t.Fatalf("the hook saw %+v, key set=%v", seen.hub, seen.invite.Set)
	}
	if left, _ := io.ReadAll(in); string(left) != rest {
		t.Fatalf("more than the first line was read: %q left", left)
	}
	if got := readConfig(t, f.stateDir())["share_existing"]; got != "none" {
		t.Fatalf("share_existing = %q, as given", got)
	}
	raw, _ := os.ReadFile(filepath.Join(f.stateDir(), "config.json"))
	secret := tok.Invite.String()[len(tok.Invite.String())-52:]
	logs := strings.Join(f.events, "\n")
	for name, s := range map[string]string{"stdout": out, "stderr": f.stderr.String(), "config": string(raw), "calls": logs} {
		if strings.Contains(s, text) || strings.Contains(s, secret) {
			t.Errorf("%s holds the token or the key:\n%s", name, s)
		}
	}
	if strings.Contains(f.stderr.String(), "--join was given on the command line") {
		t.Errorf("warned about the process list although nothing is in it: %s", f.stderr)
	}
}

// An expired invitation fails the command, names no secret, leaves no
// enrollment, and leaves the new profile sharing nothing.
func TestWorkerUpJoinFromStdinWithAnExpiredToken(t *testing.T) {
	f, _ := serviceGate(t, false, false, "build")
	f.hook("enroll", func(agent.Settings) error {
		return errors.New("the Hub refused the invitation key: it has expired (invite_expired)")
	})
	text, tok := testJoinToken(t, "https://hub.example:8443")
	out, err := stdinJoin(t, f, strings.NewReader(text+"\n"), "--share-existing", "none")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v", err)
	}
	secret := tok.Invite.String()[len(tok.Invite.String())-52:]
	for name, s := range map[string]string{"error": err.Error(), "stdout": out, "stderr": f.stderr.String()} {
		if strings.Contains(s, text) || strings.Contains(s, secret) {
			t.Errorf("%s holds the token or the key: %s", name, s)
		}
	}
	if len(f.runCalls()) != 0 {
		t.Errorf("a service was installed after a failed enrollment: %v", f.runCalls())
	}
	if s := shareOf(t, f.stateDir()); s.Mode == store.ShareAll {
		t.Errorf("the failed join left %+v", s)
	}
}

func TestJoinStdinIsBoundedAndNeverEchoed(t *testing.T) {
	f, _ := serviceGate(t, false, false, "build")
	long := "mnj1-" + strings.Repeat("A", 5000)
	for name, in := range map[string]string{
		"too long":  long,
		"empty":     "\n",
		"malformed": "mnj1-AAAA\n",
	} {
		_, err := stdinJoin(t, f, strings.NewReader(in))
		if err == nil || strings.Contains(err.Error(), "AAAA") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(f.runCalls()) != 0 {
		t.Errorf("a service was installed: %v", f.runCalls())
	}
}

func TestReadJoinTokenLineTakesOneLine(t *testing.T) {
	r := strings.NewReader("  tok  \nrest")
	got, err := readJoinTokenLine(r)
	if err != nil || got != "tok" {
		t.Fatalf("%q %v", got, err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "rest" {
		t.Fatalf("over-read: %q", rest)
	}
	if got, err := readJoinTokenLine(strings.NewReader("tok")); err != nil || got != "tok" {
		t.Fatalf("no newline: %q %v", got, err)
	}
}
