package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// The background join saves the address, the expected fingerprint and
// pairing_display=status, records that it created the expectation, and
// restarts the running Worker.
func TestJoinBackgroundSavesAndRestarts(t *testing.T) {
	c := newJoinCLI(t)
	fp := testHubFingerprint()
	m, code := c.run("", "https://hub.example:8443", "--background", "--hub-fingerprint", fp, "--share-existing", "later")
	if code != ExitOK || m["config"] != "saved" || m["activation"] != "restarted" || m["pairing_display"] != "status" ||
		m["hub_fingerprint_created"] != true || m["instance"] != "i2" {
		t.Fatalf("join --background = %v, %d\nstderr: %s", m, code, c.stderr.String())
	}
	if _, ok := m["enrollment"]; ok {
		t.Fatalf("a background join enrolls nothing itself: %v", m)
	}
	cfg := c.config()
	if cfg[config.KeyServer] != "https://hub.example:8443" || cfg[config.KeyHubFingerprint] != fp || cfg[config.KeyPairingDisplay] != "status" {
		t.Fatalf("config = %v", cfg)
	}
	pin, ok, err := agent.ReadJoinPin(c.state)
	if err != nil || !ok || !pin.CreatedFingerprint(fp) || !pin.CreatedServer("https://hub.example:8443") || pin.Source != agent.JoinPinSourceBackground {
		t.Fatalf("join pin = %+v, %v, %v", pin, ok, err)
	}
	if c.worker.restarts != 1 {
		t.Fatalf("restarts = %d", c.worker.restarts)
	}

	// Run again: the values a join created stay "created".
	if m, code := c.run("", "https://hub.example:8443", "--background", "--hub-fingerprint", fp); code != ExitOK || m["hub_fingerprint_created"] != true {
		t.Fatalf("second join = %v, %d", m, code)
	}
}

// A fingerprint provisioned in the config file before the join is not the
// join's (C11): it is kept, recorded as not created, and a different one is
// refused with nothing changed.
func TestJoinBackgroundKeepsAProvisionedFingerprint(t *testing.T) {
	c := newJoinCLI(t)
	fp := testHubFingerprint()
	f, err := config.LoadFile(filepath.Join(c.state, workerConfigName))
	if err != nil {
		t.Fatal(err)
	}
	f.Set(config.KeyHubFingerprint, fp)
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	m, code := c.run("", "https://hub.example:8443", "--background", "--share-existing", "none")
	if code != ExitOK || m["hub_fingerprint_created"] != false {
		t.Fatalf("join = %v, %d\nstderr: %s", m, code, c.stderr.String())
	}
	if pin, ok, _ := agent.ReadJoinPin(c.state); !ok || pin.HubFingerprintCreated || pin.HubFingerprint != fp {
		t.Fatalf("join pin = %+v", pin)
	}

	other, _ := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32))
	m, code = c.run("", "https://hub.example:8443", "--background", "--hub-fingerprint", string(other))
	if code != ExitError || c.config()[config.KeyHubFingerprint] != fp || c.worker.restarts != 1 {
		t.Fatalf("conflicting join = %v, %d, config %v", m, code, c.config())
	}
}

func TestJoinBackgroundWithoutLiveWorkerIs24(t *testing.T) {
	c := newJoinCLI(t)
	c.worker.running = false
	m, code := c.run("", "https://hub.example:8443", "--background", "--share-existing", "later")
	if code != ExitNotRunning || m["code"] != "not_running" || m["config"] != "saved" || m["activation"] != "not_running" {
		t.Fatalf("join = %v, %d", m, code)
	}
	if c.config()[config.KeyPairingDisplay] != "status" {
		t.Fatalf("the address is saved for the next start: %v", c.config())
	}
}

func TestJoinBackgroundWhilePausedIs25(t *testing.T) {
	c := newJoinCLI(t)
	if _, err := agent.SetPause(c.state, true, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	m, code := c.run("", "https://hub.example:8443", "--background", "--share-existing", "later")
	if code != ExitPaused || m["code"] != "paused" || c.worker.restarts != 0 {
		t.Fatalf("join while paused = %v, %d", m, code)
	}
	if _, err := os.Stat(filepath.Join(c.state, workerConfigName)); err == nil {
		t.Fatal("a paused join saved the config")
	}
	if _, ok, _ := agent.ReadJoinPin(c.state); ok {
		t.Fatal("a paused join wrote the join record")
	}
}

func TestJoinBackgroundAlreadyPairedRefuses(t *testing.T) {
	c := newJoinCLI(t)
	c.state = pairedStateDir(t)
	c.env["MINION_STATE_DIR"] = c.state
	c.worker.state = c.state
	before := c.config()
	m, code := c.run("", "https://elsewhere.example", "--background")
	if code != ExitError || m["code"] != "already_paired" || c.worker.restarts != 0 {
		t.Fatalf("join on a paired Worker = %v, %d", m, code)
	}
	if after := c.config(); after[config.KeyServer] != before[config.KeyServer] || after[config.KeyPairingDisplay] != "" {
		t.Fatalf("config changed: %v", after)
	}
}

func TestJoinBackgroundFirstRunNeedsShareChoice(t *testing.T) {
	c := newJoinCLI(t)
	m, code := c.run("", "https://hub.example:8443", "--background")
	if code != ExitUsage || c.worker.restarts != 0 {
		t.Fatalf("join = %v, %d", m, code)
	}
	if _, err := os.Stat(filepath.Join(c.state, workerConfigName)); err == nil {
		t.Fatal("a refused join saved the config")
	}
}

func TestJoinBackgroundUsage(t *testing.T) {
	c := newJoinCLI(t)
	text, _ := testJoinToken(t, "https://hub.example:8443")
	for _, args := range [][]string{
		{"--background"},
		{text, "--background"},
		{"https://hub.example", "--background", "--resume"},
		{"ftp://hub.example", "--background"},
	} {
		m, code := c.run("", args...)
		if code != ExitUsage {
			t.Fatalf("%v = %v, %d", args, m, code)
		}
		if b := c.stdout.String() + c.stderr.String(); bytes.Contains([]byte(b), []byte(text)) {
			t.Fatal("the token was echoed")
		}
	}
}
