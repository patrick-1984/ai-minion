package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestHubServeIsRegisteredAndTakesNoArguments(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute(context.Background(), []string{"hub", "serve", "extra"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "unknown command") && !strings.Contains(errb.String(), "accepts 0 arg") {
		t.Errorf("stderr = %q", errb.String())
	}
	root := NewRoot()
	cmd, _, err := root.Find([]string{"hub", "serve"})
	if err != nil || cmd.Name() != "serve" {
		t.Fatalf("hub serve not found: %v", err)
	}
	if cmd.Flags().HasFlags() {
		t.Error("hub serve has flags; the Hub reads its settings from the environment")
	}
}

func TestHubCommandIsSharedWithOtherSubcommands(t *testing.T) {
	root := &cobra.Command{Use: "minion"}
	existing := &cobra.Command{Use: "hub", Short: "from another story"}
	root.AddCommand(existing)
	if got := hubCommand(root); got != existing {
		t.Fatal("hubCommand created a second hub command")
	}
	if n := len(root.Commands()); n != 1 {
		t.Fatalf("root has %d commands", n)
	}
	fresh := &cobra.Command{Use: "minion"}
	a := hubCommand(fresh)
	b := hubCommand(fresh)
	if a != b || len(fresh.Commands()) != 1 {
		t.Fatal("hubCommand is not idempotent")
	}
}

func TestHubServeRefusesTLSOffWithoutPublicURL(t *testing.T) {
	var errb lockedBuf
	err := runHubServe(context.Background(), hubServeDeps{
		stderr:    &errb,
		lookupEnv: envOf(map[string]string{"MINION_DATA_DIR": t.TempDir(), "MINION_TLS": "off", "MINION_LISTEN": "127.0.0.1:0"}),
	})
	if err == nil || !strings.Contains(err.Error(), "MINION_PUBLIC_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestHubServeRejectsInvalidSettings(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"tls value":     {"MINION_TLS": "maybe"},
		"half a pair":   {"MINION_TLS_CERT_FILE": "/c.pem"},
		"proxy list":    {"MINION_TRUSTED_PROXIES": "nope"},
		"listen format": {"MINION_LISTEN": "8443"},
		"public url":    {"MINION_PUBLIC_URL": "hub"},
	} {
		t.Run(name, func(t *testing.T) {
			env["MINION_DATA_DIR"] = t.TempDir()
			err := runHubServe(context.Background(), hubServeDeps{stderr: io.Discard, lookupEnv: envOf(env)})
			if err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestHubServeRunsOverTLS13 is the integration test of the story: hub serve
// on a random port with a temporary data dir, both health endpoints over TLS
// 1.3, and a TLS 1.2 client is rejected.
func TestHubServeRunsOverTLS13(t *testing.T) {
	var errb lockedBuf
	addrCh := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	data := t.TempDir()
	go func() {
		done <- runHubServe(ctx, hubServeDeps{
			stderr:    &errb,
			lookupEnv: envOf(map[string]string{"MINION_DATA_DIR": data, "MINION_LISTEN": "127.0.0.1:0", "MINION_LOG_FORMAT": "text"}),
			started:   func(a string) { addrCh <- a },
		})
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("hub serve ended early: %v\n%s", err, errb.String())
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("hub serve did not start")
	}

	client := func(maxVersion uint16) *http.Client {
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, MaxVersion: maxVersion}, //nolint:gosec // self-signed test server
		}}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := client(0).Get("https://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.TLS.Version != tls.VersionTLS13 {
			t.Errorf("%s = %d over TLS %#x", path, resp.StatusCode, resp.TLS.Version)
		}
	}
	if resp, err := client(tls.VersionTLS12).Get("https://" + addr + "/healthz"); err == nil {
		resp.Body.Close()
		t.Error("a TLS 1.2 client was served")
	}
	if !strings.Contains(errb.String(), "fingerprint_sha256=") {
		t.Errorf("the log does not show the certificate fingerprint:\n%s", errb.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hub serve returned %v on shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("hub serve did not stop")
	}
}

func TestHubServeUnwritableDataDirExitsWithFix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the chown hint is printed on Unix only")
	}
	// A data dir below a regular file can never be created or written, as
	// root as well.
	var out, errb bytes.Buffer
	file := t.TempDir() + "/file"
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINION_DATA_DIR", file+"/data")
	t.Setenv("MINION_LISTEN", "127.0.0.1:0")
	if code := Execute(context.Background(), []string{"hub", "serve"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	dir := file + "/data"
	for _, want := range []string{
		"data dir " + dir + " does not exist and uid ",
		" cannot create it; run: mkdir -p <host dir> && chown ",
		"<host dir> (or mount a host directory at " + dir + ", or use an init container)",
	} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr = %q\nwant it to contain %q", errb.String(), want)
		}
	}
	if strings.Contains(errb.String(), "is not writable") {
		t.Errorf("a missing dir is reported as unwritable: %q", errb.String())
	}
	// No path outside the data dir: the regular file is only ever printed as
	// the prefix of the data dir.
	if strings.Count(errb.String(), file) != strings.Count(errb.String(), dir) {
		t.Errorf("stderr prints a path outside the data dir: %q", errb.String())
	}
}
