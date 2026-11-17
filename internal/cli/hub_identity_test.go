package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/server"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

func TestHubIdentityIsRegisteredAndTakesNoArguments(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"hub", "identity"})
	if err != nil || cmd.Name() != "identity" {
		t.Fatalf("hub identity not found: %v", err)
	}
	var out, errb bytes.Buffer
	if code := Execute(context.Background(), []string{"hub", "identity", "extra"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestHubIdentityPrintsTheFingerprintOfTheKeyInTheDataDir(t *testing.T) {
	dir := t.TempDir()
	// A 0.6 Hub whose passphrase is not set yet (state B1) keeps its plain
	// root in identity.key.
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.SaveKey(filepath.Join(dir, workers.IdentityFile), k); err != nil {
		t.Fatal(err)
	}
	id := k
	var out bytes.Buffer
	if err := runHubIdentity(&out, envOf(map[string]string{"MINION_DATA_DIR": dir}), false); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(out.String())
	if got != id.Fingerprint().String() || len(got) != 52 {
		t.Errorf("printed %q, want %q", got, id.Fingerprint())
	}
	// Reading never changes the identity.
	var again bytes.Buffer
	if err := runHubIdentity(&again, envOf(map[string]string{"MINION_DATA_DIR": dir}), false); err != nil || again.String() != out.String() {
		t.Errorf("second run printed %q err=%v", again.String(), err)
	}
}

func TestHubIdentityBeforeThePassphraseSaysSoAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runHubIdentity(&out, envOf(map[string]string{"MINION_DATA_DIR": dir}), false)
	if err == nil || !strings.Contains(err.Error(), "minion hub identity set-passphrase") || strings.Contains(err.Error(), "first starts") {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(matches) != 0 {
		t.Errorf("the command created %v", matches)
	}
}

// F4: after set-passphrase the private key file is gone; the bare command
// still prints the fingerprint, read from the database, with no Hub running.
func TestHubIdentityWorksAfterSealingWithoutKeyFileOrSocket(t *testing.T) {
	dir := t.TempDir()
	// An established Hub (B3): sealed root, no identity.key, no socket.
	rootPub := hubidtest.Provision(t, dir)
	if _, err := os.Stat(filepath.Join(dir, workers.IdentityFile)); !os.IsNotExist(err) {
		t.Fatalf("identity.key present: %v", err)
	}
	want, err := identity.FingerprintOf(rootPub)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHubIdentity(&out, envOf(map[string]string{"MINION_DATA_DIR": dir}), false); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != want.String() {
		t.Errorf("printed %q, want %q", got, want)
	}
}

// F6: a signal during a passphrase read restores the terminal and ends the
// command with a non-zero status.
func TestPassphraseReadIsCancellableAndRestoresTheTerminal(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	restored := make(chan struct{})
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	go func() { sigs <- os.Interrupt }()
	s, err := readCancellable(sigs, func() { close(restored) }, func() (string, error) {
		<-block
		return "never", nil
	})
	if !errors.Is(err, errInterrupted) || s != "" {
		t.Fatalf("got %q, %v", s, err)
	}
	select {
	case <-restored:
	default:
		t.Error("the terminal state was not restored")
	}
	// A normal read returns its value.
	if s, err := readCancellable(make(chan os.Signal), nil, func() (string, error) { return "x", nil }); s != "x" || err != nil {
		t.Errorf("normal read = %q, %v", s, err)
	}
	// The command maps the interrupt to a non-zero exit code.
	run := &identityRun{deps: identityDeps{readSecret: func(string) (string, error) { return "", errInterrupted }}}
	_, err = run.secret("p: ")
	var ee *exitCodeError
	if !errors.As(err, &ee) || ee.code != ExitInterrupted {
		t.Errorf("secret err = %v", err)
	}
}

// W2: --words prints block M10 with the words, the 52 characters in every
// copy line, the public URL when one is set, and the TLS key pin of the
// Hub's own certificate.
func TestHubIdentityWordsPrintsBlockM10(t *testing.T) {
	dir := t.TempDir()
	rootPub := hubidtest.Provision(t, dir)
	fp, err := identity.FingerprintOf(rootPub)
	if err != nil {
		t.Fatal(err)
	}
	certs, err := server.LoadCertSource(server.CertOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	pin := server.TLSKeyPin(certs.KeyHash())

	var out bytes.Buffer
	env := map[string]string{"MINION_DATA_DIR": dir, "MINION_PUBLIC_URL": "https://minion.example.com/"}
	if err := runHubIdentity(&out, envOf(env), true); err != nil {
		t.Fatal(err)
	}
	full := fp.String()
	want := "Hub fingerprint:  " + fp.Words() + "\n" +
		"Full:             " + full + "\n\n" +
		"For a new Worker (it then accepts only this Hub):\n" +
		"  minion worker join https://minion.example.com --hub-fingerprint " + full + "\n" +
		"  MINION_HUB_FINGERPRINT=" + full + "\n" +
		"For a Worker that is already paired, run on the Worker:\n" +
		"  minion worker verify-hub " + full + "\n\n" +
		"TLS key pin: " + pin + "\n"
	if !strings.HasPrefix(out.String(), want) || !strings.Contains(out.String(), "--hub-tls-pin") {
		t.Errorf("printed\n%s\nwant prefix\n%s", out.String(), want)
	}
	if len(strings.Split(fp.Words(), "-")) != 6 || !strings.HasPrefix(pin, "sha256//") || len(pin) != len("sha256//")+44 {
		t.Errorf("words %q pin %q", fp.Words(), pin)
	}

	// Without a public URL the placeholder; with MINION_TLS=off no pin.
	out.Reset()
	if err := runHubIdentity(&out, envOf(map[string]string{"MINION_DATA_DIR": dir, "MINION_TLS": "off"}), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  minion worker join <hub address> --hub-fingerprint "+full+"\n") || strings.Contains(out.String(), "TLS key pin") {
		t.Errorf("printed\n%s", out.String())
	}
}

func TestHubIdentityWordsWithoutCertificateFileHasNoPin(t *testing.T) {
	dir := t.TempDir()
	hubidtest.Provision(t, dir)
	var out bytes.Buffer
	if err := runHubIdentity(&out, envOf(map[string]string{"MINION_DATA_DIR": dir}), true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "TLS key pin") || !strings.Contains(out.String(), "Hub fingerprint:  ") {
		t.Errorf("printed\n%s", out.String())
	}
}

func TestHubIdentityWordsBeforeThePassphrasePrintsNothing(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runHubIdentity(&out, envOf(map[string]string{"MINION_DATA_DIR": dir, "MINION_PUBLIC_URL": "https://h.example"}), true)
	if err == nil || !strings.Contains(err.Error(), "minion hub identity set-passphrase") {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}
