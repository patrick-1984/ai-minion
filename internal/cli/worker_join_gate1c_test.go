package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// hookReader runs f once, when the first read happens (the token is "typed"
// at that moment), then reads from r.
type hookReader struct {
	r    io.Reader
	f    func()
	done bool
}

func (h *hookReader) Read(p []byte) (int, error) {
	if !h.done {
		h.done = true
		h.f()
	}
	return h.r.Read(p)
}

// An owner pause that lands while the join waits for the token stops the
// join before the Hub is dialed.
func TestJoinPausedWhileWaitingForTokenDoesNotDial(t *testing.T) {
	c := newJoinCLI(t)
	text, _ := testJoinToken(t, "https://hub.example:8443")
	hr := &hookReader{r: strings.NewReader(text), f: func() {
		if _, err := agent.SetPause(c.state, true, time.Time{}, time.Now()); err != nil {
			t.Error(err)
		}
	}}
	// run() resets stdin from a string: drive it through a wrapper instead.
	m, code := c.runReader(hr, "--token-file", "-", "--share-existing", "all")
	if code != ExitPaused || c.invite.n() != 0 {
		t.Fatalf("join = %v, %d (invites %d)", m, code, c.invite.n())
	}
}

// A hub_fingerprint provisioned while the join waits for the token is never
// replaced.
func TestJoinFingerprintProvisionedWhileWaitingIsKept(t *testing.T) {
	c := newJoinCLI(t)
	text, _ := testJoinToken(t, "https://hub.example:8443")
	other, _ := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32))
	hr := &hookReader{r: strings.NewReader(text), f: func() {
		f, err := config.LoadFile(filepath.Join(c.state, workerConfigName))
		if err != nil {
			t.Error(err)
			return
		}
		f.Set(config.KeyHubFingerprint, string(other))
		if err := f.Save(); err != nil {
			t.Error(err)
		}
	}}
	m, code := c.runReader(hr, "--token-file", "-", "--share-existing", "all")
	if code == ExitOK || c.invite.n() != 0 {
		t.Fatalf("join = %v, %d (invites %d)", m, code, c.invite.n())
	}
	if got := c.config()[config.KeyHubFingerprint]; got != string(other) {
		t.Fatalf("the pin was overwritten: %q", got)
	}
	// The save helper alone also refuses to replace a different pin.
	if _, err := saveJoinHub(c.state, "https://hub.example:8443", testHubFingerprint(), agent.JoinPinSourceBackground, time.Now()); err == nil {
		t.Fatal("saveJoinHub replaced a different pin")
	}
}

// A refused resume (another Hub) leaves the sharing choice as it was.
func TestResumeRefusedDoesNotChangeSharing(t *testing.T) {
	c := newJoinCLI(t)
	text, _ := testJoinToken(t, "https://hub.example:8443")
	if _, code := c.run(text, "--token-file", "-", "--share-existing", "none"); code != ExitOK {
		t.Fatalf("join: %d %s", code, c.stderr.String())
	}
	before := c.share()
	if err := saveWorkerHub(c.state, "https://other.example:8443", ""); err != nil {
		t.Fatal(err)
	}
	m, code := c.run("", "--resume", "--share-existing", "all")
	if code != ExitStateChanged {
		t.Fatalf("resume = %v, %d", m, code)
	}
	if after := c.share(); after.Mode != store.ShareNone {
		t.Fatalf("sharing changed by a refused resume: %+v -> %+v", before, after)
	}
}

// A share_existing from the Worker's environment would undo the choice at the
// restart: the join refuses (22) before it enrolls.
func TestJoinRefusesWhenStartupShareRuleWouldUndoChoice(t *testing.T) {
	c := newJoinCLI(t)
	c.env["MINION_SHARE_EXISTING"] = "all"
	text, _ := testJoinToken(t, "https://hub.example:8443")
	m, code := c.run(text, "--token-file", "-", "--share-existing", "none")
	if code != ExitNotInForce || c.invite.n() != 0 || c.worker.restarts != 0 {
		t.Fatalf("join = %v, %d (invites %d, restarts %d)", m, code, c.invite.n(), c.worker.restarts)
	}
}

// A console pairing display from a flag or the environment is refused: the
// code would reach the console and the log.
func TestJoinBackgroundRefusesConsoleDisplay(t *testing.T) {
	c := newJoinCLI(t)
	c.env["MINION_PAIRING_DISPLAY"] = "console"
	m, code := c.run("", "https://hub.example:8443", "--background", "--share-existing", "later")
	if code != ExitError || c.worker.restarts != 0 {
		t.Fatalf("join --background = %v, %d", m, code)
	}
	if _, ok := c.config()[config.KeyServer]; ok {
		t.Fatalf("config was changed: %v", c.config())
	}
}

// The provenance record is written before the config file: when it cannot be
// written, the config holds nothing the record does not know of.
func TestJoinBackgroundRecordFailureLeavesConfigUntouched(t *testing.T) {
	c := newJoinCLI(t)
	if err := os.MkdirAll(filepath.Join(c.state, agent.JoinPinFile, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, code := c.run("", "https://hub.example:8443", "--background", "--hub-fingerprint", testHubFingerprint(), "--share-existing", "later")
	if code == ExitOK {
		t.Fatalf("join --background = %v", m)
	}
	if cfg := c.config(); cfg[config.KeyServer] != "" || cfg[config.KeyHubFingerprint] != "" {
		t.Fatalf("config = %v although the record failed", cfg)
	}
}
