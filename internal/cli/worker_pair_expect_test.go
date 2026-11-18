package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// The fake Hub of newPairCLI answers with the key of 32 bytes 0x09.
func fakeHubFingerprint(t *testing.T) identity.Fingerprint {
	t.Helper()
	f, err := identity.FingerprintOf(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWorkerPairWithHubFingerprintStoresTheCheck(t *testing.T) {
	fp := fakeHubFingerprint(t)
	c := newPairCLI(t, nil)
	if err := c.exec("pair", "--hub-fingerprint", fp.Words()); err != nil {
		t.Fatal(err)
	}
	if got := c.dialer.calls[0].ExpectHub; !got.MatchesFingerprint(fp) {
		t.Fatal("the expectation did not reach the pairing")
	}
	enr, ok := c.enrollment()
	if !ok || enr.Check() != store.HubCheckFingerprint {
		t.Fatalf("enrollment %+v", enr)
	}
	if !strings.Contains(c.stdout.String(), "It matched the fingerprint you gave.") {
		t.Fatal(c.stdout.String())
	}
}

func TestWorkerPairInvalidFingerprintChangesNothing(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair() // a first pairing, so that --new-code has something to renew
	before := c.keyPub()
	err := c.exec("join", "https://other.example", "--new-code", "--yes", "--hub-fingerprint", "abc")
	if err == nil || !strings.Contains(err.Error(), "hub_fingerprint") {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(before, c.keyPub()) {
		t.Fatal("the key was renewed before the fingerprint was checked")
	}
	if b, _ := os.ReadFile(filepath.Join(c.state, workerConfigName)); strings.Contains(string(b), "other.example") {
		t.Fatal("the address was saved")
	}
	if c.dialer.n() != 1 {
		t.Fatal("the Hub was contacted")
	}
}

func TestWorkerJoinSavesAddressAndFingerprintTogether(t *testing.T) {
	fp := fakeHubFingerprint(t)
	c := newPairCLI(t, map[string]string{"MINION_SERVER": ""})
	if err := c.exec("join", "https://hub.example:8443", "--hub-fingerprint", string(fp)); err != nil {
		t.Fatal(err)
	}
	f, err := config.LoadFile(filepath.Join(c.state, workerConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := f.Lookup(config.KeyServer); v != "https://hub.example:8443" {
		t.Fatalf("server = %q", v)
	}
	if v, _, _ := f.Lookup(config.KeyHubFingerprint); v != string(fp) {
		t.Fatalf("hub_fingerprint = %q", v)
	}
}

func TestWorkerPairAsksForTheFingerprintFirst(t *testing.T) {
	fp := fakeHubFingerprint(t)
	c := newPairCLI(t, nil)
	c.pair()
	// --new-code on a paired Worker: the fingerprint is asked before the
	// confirmation, and the answer to the confirmation follows it.
	c.deps.stdin = strings.NewReader(fp.Words() + "\ny\n")
	c.deps.interactive = func(io.Reader, io.Writer) bool { return true }
	c.stdout.Reset()
	if err := c.exec("pair", "--new-code"); err != nil {
		t.Fatal(err)
	}
	out := c.stdout.String()
	ask, confirm := strings.Index(out, "NEW HUB - ENTER ITS FINGERPRINT"), strings.Index(out, "Continue? [y/N]")
	if ask < 0 || confirm < 0 || ask > confirm {
		t.Fatalf("the fingerprint must be asked before the confirmation:\n%s", out)
	}
	if enr, _ := c.enrollment(); enr.Check() != store.HubCheckFingerprint {
		t.Fatalf("enrollment %+v", enr)
	}

	// End of input at the prompt aborts and changes nothing.
	c2 := newPairCLI(t, nil)
	c2.deps.interactive = func(io.Reader, io.Writer) bool { return true }
	if err := c2.exec("pair"); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("err = %v", err)
	}
	if c2.dialer.n() != 0 {
		t.Fatal("the Hub was contacted before the answer")
	}
}

func TestWorkerPairWithoutTerminalDoesNotAsk(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	if strings.Contains(c.stdout.String(), "ENTER ITS FINGERPRINT") {
		t.Fatal("asked without a terminal")
	}
	if enr, _ := c.enrollment(); enr.Check() != store.HubCheckNone {
		t.Fatalf("enrollment %+v", enr)
	}
}
