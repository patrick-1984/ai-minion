package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

var plainBackup = hubBackupOpts{unencrypted: true}

func TestHubBackupAndRestoreAreRegistered(t *testing.T) {
	root := NewRoot()
	for _, name := range []string{"backup", "restore"} {
		cmd, _, err := root.Find([]string{"hub", name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("hub %s: %v", name, err)
		}
	}
	var out, errb bytes.Buffer
	if code := Execute(context.Background(), []string{"hub", "backup"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "out") {
		t.Fatalf("backup without --out: code %d stderr %q", code, errb.String())
	}
	errb.Reset()
	if code := Execute(context.Background(), []string{"hub", "restore"}, &out, &errb); code != 1 {
		t.Fatalf("restore without archive: code %d", code)
	}
}

func TestHubBackupThenRestoreIntoNewDir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "hub")
	s, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(context.Background(), "k", "v", false, "t"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	identity := []byte("identity-key-bytes")
	if err := os.WriteFile(filepath.Join(src, store.IdentityKeyFile), identity, 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "hub.tar.gz")

	// The data dir comes from the environment, like "minion hub serve".
	d := hubBackupDeps{lookupEnv: envOf(map[string]string{"MINION_DATA_DIR": src})}
	var out bytes.Buffer
	if err := runHubBackup(context.Background(), &out, d, "", archive, plainBackup); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Backup written to "+archive) || !strings.Contains(out.String(), "secret keys") {
		t.Fatalf("output = %q", out.String())
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(archive); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("archive: %v %v", st, err)
		}
	}
	if err := runHubBackup(context.Background(), &out, d, "", archive, plainBackup); err == nil {
		t.Fatal("second backup overwrote the archive")
	}

	dst := filepath.Join(t.TempDir(), "new")
	out.Reset()
	hostOnly := hubBackupDeps{containerDir: func(string) containerKind { return notContainer }}
	if err := runHubRestore(context.Background(), &out, hostOnly, dst, archive, false, "", 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Restored 3 files into "+dst) || strings.Contains(out.String(), "chown") {
		t.Fatalf("output = %q", out.String())
	}
	got, err := os.ReadFile(filepath.Join(dst, store.IdentityKeyFile))
	if err != nil || !bytes.Equal(got, identity) {
		t.Fatalf("identity file after restore: %q %v", got, err)
	}
	r, err := store.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, ok, err := r.GetSetting(context.Background(), "k"); err != nil || !ok {
		t.Fatalf("setting after restore: ok=%v err=%v", ok, err)
	}

	// A non-empty dir needs --force.
	err = runHubRestore(context.Background(), &out, hostOnly, dst, archive, false, "", 0)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if err := runHubRestore(context.Background(), &out, hostOnly, dst, archive, true, "", 0); err != nil {
		t.Fatal(err)
	}
}

func TestHubBackupNeedsADataDir(t *testing.T) {
	d := hubBackupDeps{lookupEnv: envOf(map[string]string{"MINION_DATA_DIR": filepath.Join(t.TempDir(), "none")})}
	err := runHubBackup(context.Background(), &bytes.Buffer{}, d, "", filepath.Join(t.TempDir(), "x"), plainBackup)
	if err == nil || !strings.Contains(err.Error(), "not a Hub data directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestLooksLikeContainerDir(t *testing.T) {
	none := func(string) bool { return false }
	only := func(p string) func(string) bool { return func(q string) bool { return q == p } }
	if runtime.GOOS == "windows" {
		t.Skip("no container reminder on Windows")
	}
	cases := []struct {
		name string
		dir  string
		euid int
		ex   func(string) bool
		want containerKind
	}{
		{"host user, host dir", "/home/me/hub", 1000, none, notContainer},
		{"image data dir on the host", "/data", 1000, none, onContainerHost},
		{"image data dir, unclean", "/data/", 1000, none, onContainerHost},
		{"root restoring on the host", "/srv/hub", 0, none, onContainerHost},
		{"inside docker", "/srv/hub", 1000, only("/.dockerenv"), insideContainer},
		{"inside podman", "/srv/hub", 1000, only("/run/.containerenv"), insideContainer},
		{"inside docker as root", "/data", 0, only("/.dockerenv"), insideContainer},
		{"inside docker, image data dir", "/data", 1000, only("/.dockerenv"), insideContainer},
		{"already the Hub user, image data dir", "/data", containerUID, none, notContainer},
		{"already the Hub user, inside docker", "/data", containerUID, only("/.dockerenv"), notContainer},
	}
	for _, c := range cases {
		if got := looksLikeContainerDir(c.dir, c.euid, c.ex); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRestorePrintsChownReminderForContainerDir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "hub")
	s, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := runHubBackup(context.Background(), &bytes.Buffer{}, hubBackupDeps{}, src, archive, plainBackup); err != nil {
		t.Fatal(err)
	}
	restore := func(kind containerKind) string {
		t.Helper()
		dst := filepath.Join(t.TempDir(), "d")
		var out bytes.Buffer
		d := hubBackupDeps{containerDir: func(string) containerKind { return kind }}
		if err := runHubRestore(context.Background(), &out, d, dst, archive, false, "", 0); err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(out.String(), dst, "<DST>")
	}

	// On the host the restored directory is a host path.
	if got := restore(onContainerHost); !strings.Contains(got, "on the host run:\n  chown -R 65532:65532 <DST>\n") {
		t.Errorf("host output = %q", got)
	}
	// Inside a container the path is the container's view of a mount: the
	// literal path must not be offered to chown.
	got := restore(insideContainer)
	if !strings.Contains(got, "chown -R 65532:65532 <host dir>") ||
		!strings.Contains(got, "run it on the host directory mounted at <DST>") ||
		strings.Contains(got, "chown -R 65532:65532 <DST>") {
		t.Errorf("container output = %q", got)
	}
	if got := restore(notContainer); strings.Contains(got, "chown") {
		t.Errorf("output without a container = %q", got)
	}
}

func TestResolveDataDirTrimsWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name, flag, env, want string
	}{
		{"env with trailing space", "", "/data ", "/data"},
		{"env with CR", "", "/data\r", "/data"},
		{"env with surrounding whitespace", "", "  /srv/hub \t", "/srv/hub"},
		{"flag with whitespace", " /flag/dir\r\n", "/env/dir", "/flag/dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := hubBackupDeps{lookupEnv: envOf(map[string]string{"MINION_DATA_DIR": tc.env})}
			got, err := resolveDataDir(tc.flag, d)
			if err != nil || got != tc.want {
				t.Fatalf("resolveDataDir = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	// A value of only whitespace is "not set".
	d := hubBackupDeps{lookupEnv: envOf(map[string]string{"MINION_DATA_DIR": " \r\n"})}
	if _, err := resolveDataDir("", d); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreIntoTrimmedDirDoesNotCreateASiblingDir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "hub")
	s, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := runHubBackup(context.Background(), &bytes.Buffer{}, hubBackupDeps{}, src, archive, plainBackup); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	dst := filepath.Join(parent, "data")
	d := hubBackupDeps{
		lookupEnv:    envOf(map[string]string{"MINION_DATA_DIR": dst + " \r"}),
		containerDir: func(string) containerKind { return notContainer },
	}
	if err := runHubRestore(context.Background(), &bytes.Buffer{}, d, "", archive, false, "", 0); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "data" {
		t.Fatalf("parent holds %v (err %v), want only \"data\"", entries, err)
	}
}

// startHub runs "minion hub serve" on data (TLS on, random port) and returns
// its address, a stop function that waits for a clean shutdown and the
// buffer that captures its log.
func startHub(t *testing.T, data string) (addr string, stop func(), logs *lockedBuf) {
	t.Helper()
	errb := &lockedBuf{}
	addrCh := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runHubServe(ctx, hubServeDeps{
			stderr:    errb,
			lookupEnv: envOf(map[string]string{"MINION_DATA_DIR": data, "MINION_LISTEN": "127.0.0.1:0", "MINION_LOG_FORMAT": "text"}),
			started:   func(a string) { addrCh <- a },
		})
	}()
	select {
	case addr = <-addrCh:
	case err := <-done:
		cancel()
		t.Fatalf("hub serve ended early: %v\n%s", err, errb.String())
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("hub serve did not start")
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("hub serve returned %v on shutdown", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("hub serve did not stop")
		}
	}
	t.Cleanup(stop)
	return addr, stop, errb
}

// presentedLeafSHA256 returns the SHA-256 of the leaf certificate the Hub at
// addr presents.
func presentedLeafSHA256(t *testing.T, addr string) [sha256.Size]byte {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr,
		&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // self-signed test server
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatal("the Hub presented no certificate")
	}
	return sha256.Sum256(certs[0].Raw)
}

func readyz(t *testing.T, addr string) int {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test server
	}}
	resp, err := c.Get("https://" + addr + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestBackupRestoreThenHubStarts is the integration test of the story: back
// up a running Hub, restore into a new directory, and start the Hub on it. It
// presents the same TLS certificate, which proves the restored tls/ files are
// the ones the server loads (a drifted file name would make it generate a new
// certificate), the identity file is byte-identical, and the running restored
// Hub logs the "hub identity" record with the fingerprint of the first Hub
// and no "hub identity created" record, so it loaded the restored identity
// and did not generate a new one (Story 4.3).
func TestBackupRestoreThenHubStarts(t *testing.T) {
	data := filepath.Join(t.TempDir(), "hub")
	addr, stop, _ := startHub(t, data)
	if got := readyz(t, addr); got != 200 {
		t.Fatalf("/readyz of the first Hub = %d", got)
	}
	// The first start generates the Hub identity (Story 4.3).
	identity, err := os.ReadFile(filepath.Join(data, store.IdentityKeyFile))
	if err != nil || len(identity) == 0 {
		t.Fatalf("the first Hub did not create its identity key: %v", err)
	}
	id, err := workers.ReadIdentity(data)
	if err != nil {
		t.Fatal(err)
	}
	wantFingerprint := id.Fingerprint().String()
	wantCert := presentedLeafSHA256(t, addr)

	// Backup while the Hub is running.
	archive := filepath.Join(t.TempDir(), "hub.tar.gz")
	var out bytes.Buffer
	d := hubBackupDeps{lookupEnv: envOf(map[string]string{"MINION_DATA_DIR": data})}
	pf := writePassphraseFile(t, "integration passphrase\n")
	if err := runHubBackup(context.Background(), &out, d, "", archive, hubBackupOpts{passphraseFile: pf}); err != nil {
		t.Fatalf("backup of a running Hub: %v", err)
	}
	if got := readyz(t, addr); got != 200 {
		t.Fatalf("/readyz after the backup = %d", got)
	}
	stop()

	// Restore into a new directory and start the Hub on it.
	restored := filepath.Join(t.TempDir(), "restored")
	hostOnly := hubBackupDeps{containerDir: func(string) containerKind { return notContainer }}
	if raw, err := os.ReadFile(archive); err != nil || !strings.HasPrefix(string(raw), "age-encryption.org/v1") {
		t.Fatalf("the archive is not age-encrypted: %v", err)
	}
	if err := runHubRestore(context.Background(), &out, hostOnly, restored, archive, false, pf, 0); err != nil {
		t.Fatal(err)
	}
	addr2, stop2, logs2 := startHub(t, restored)
	defer stop2()
	if got := readyz(t, addr2); got != 200 {
		t.Fatalf("/readyz of the restored Hub = %d", got)
	}
	if got := presentedLeafSHA256(t, addr2); got != wantCert {
		t.Fatalf("the restored Hub presents another certificate: %x, want %x", got, wantCert)
	}
	log := logs2.String()
	loaded := false
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, `msg="hub identity"`) && strings.Contains(line, "fingerprint="+wantFingerprint) {
			loaded = true
		}
	}
	if !loaded {
		t.Fatalf("the restored Hub did not log \"hub identity\" with fingerprint %s:\n%s", wantFingerprint, log)
	}
	if strings.Contains(log, `msg="hub identity created"`) {
		t.Fatalf("the restored Hub created a new identity:\n%s", log)
	}
	got, err := os.ReadFile(filepath.Join(restored, store.IdentityKeyFile))
	if err != nil || !bytes.Equal(got, identity) {
		t.Fatalf("identity file after restore: %q %v", got, err)
	}
}
