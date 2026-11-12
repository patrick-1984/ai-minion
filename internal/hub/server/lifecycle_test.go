package server

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// ---- readiness check of the real store module (AD-16: no SQL here) ---------

func TestStoreModuleReadyCheckFollowsTheDatabase(t *testing.T) {
	var (
		st     *store.Store
		health *Health
	)
	reg := registryWith(t, true, nil)
	err := reg.Register("grab", 60, func(_ context.Context, env *Env) error {
		var err error
		if st, err = Require[*store.Store](env); err != nil {
			return err
		}
		health, err = Require[*Health](env)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: hubstore.Dir(t), TLSOff: true, PublicURL: "http://hub.example"}, reg)
	url := "http://" + h.srv.Addr() + "/readyz"

	if failing, ok := health.Ready(context.Background()); !ok {
		t.Fatalf("Ready with an open database = (%q, false)", failing)
	}
	if code, body, _ := getBody(t, http.DefaultClient, url); code != 200 {
		t.Fatalf("/readyz with an open database = %d %s", code, body)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if failing, ok := health.Ready(context.Background()); ok || failing != "database" {
		t.Fatalf("Ready with a closed database = (%q, %v), want (\"database\", false)", failing, ok)
	}
	code, body, _ := getBody(t, http.DefaultClient, url)
	if code != 503 || !strings.Contains(body, `"reason":"database"`) {
		t.Fatalf("/readyz with a closed database = %d %s", code, body)
	}
	if strings.Contains(strings.ToLower(body), "closed") || strings.Contains(body, "sql") {
		t.Errorf("/readyz leaks internal detail: %s", body)
	}
}

// ---- package-level helpers while modules initialise ------------------------

func TestPublicURLIsSetWhileModulesInitialiseAndStart(t *testing.T) {
	const public = "https://hub.example.com"
	var inInit, inStart atomic.Value
	reg := registryWith(t, false, nil)
	err := reg.Register("probe-url", 60, func(_ context.Context, env *Env) error {
		inInit.Store(PublicURL())
		env.OnStart(func(context.Context) error {
			inStart.Store(PublicURL())
			return nil
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	startHub(t, &Config{DataDir: hubstore.Dir(t), TLSOff: true, PublicURL: public}, reg)
	if got, _ := inInit.Load().(string); got != public {
		t.Errorf("server.PublicURL() in init = %q, want %q", got, public)
	}
	if got, _ := inStart.Load().(string); got != public {
		t.Errorf("server.PublicURL() in the start hook = %q, want %q", got, public)
	}
}

// ---- health endpoints answer while the modules initialise ------------------

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestHealthEndpointsAnswerWhileInitialisationIsBlocked(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	reg := registryWith(t, false, nil)
	err := reg.Register("slow", 60, func(ctx context.Context, env *Env) error {
		mux, err := Require[*http.ServeMux](env)
		if err != nil {
			return err
		}
		mux.HandleFunc("GET /app", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "app") })
		once.Do(func() { close(entered) })
		select { // a long migration
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	addr := freeAddr(t)
	srv := New(&Config{DataDir: hubstore.Dir(t), Listen: addr, TLSOff: true, PublicURL: "http://hub.example", Logger: testLogger(io.Discard)}, reg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v after shutdown", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("the Hub did not stop")
		}
	})

	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("initialisation never started")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) (int, string) {
		t.Helper()
		var lastErr error
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			resp, err := client.Get("http://" + addr + path)
			if err != nil {
				lastErr = err
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp.StatusCode, string(b)
		}
		t.Fatalf("GET %s while initialising: %v", path, lastErr)
		return 0, ""
	}
	if code, body := get("/healthz"); code != 200 {
		t.Errorf("/healthz during initialisation = %d %s, want 200", code, body)
	}
	if code, body := get("/readyz"); code != 503 || !strings.Contains(body, `"reason":"starting"`) {
		t.Errorf("/readyz during initialisation = %d %s, want 503 starting", code, body)
	}
	if code, _ := get("/app"); code != 503 {
		t.Errorf("an application route during initialisation = %d, want 503", code)
	}
	select {
	case <-srv.Started():
		t.Fatal("the Hub reports started while a module is still initialising")
	default:
	}

	close(release)
	select {
	case <-srv.Started():
	case err := <-done:
		t.Fatalf("Run returned %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the Hub did not start after initialisation finished")
	}
	if code, _ := get("/readyz"); code != 200 {
		t.Errorf("/readyz after startup = %d", code)
	}
	if code, body := get("/app"); code != 200 || body != "app" {
		t.Errorf("an application route after startup = %d %q", code, body)
	}
}

func TestFailedInitialisationStopsTheListener(t *testing.T) {
	reg := registryWith(t, false, nil)
	if err := reg.Register("broken", 60, func(context.Context, *Env) error { return errors.New("boom") }); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	srv := New(&Config{DataDir: hubstore.Dir(t), Listen: addr, TLSOff: true, PublicURL: "http://hub.example", Logger: testLogger(io.Discard)}, reg)
	if err := srv.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Run = %v", err)
	}
	if PublicURL() != "" {
		t.Error("the package-level public URL outlived a failed start")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the listen address is still bound after a failed start: %v", err)
	}
	ln.Close()
}

// ---- X-Forwarded-Proto from a trusted proxy over a TLS backend -------------

func TestSchemeHonoursTrustedProxyOverBackendTLS(t *testing.T) {
	n, _ := NewNetwork("", true, mustPrefixes(t, "10.0.0.0/8"))
	withTLS := func(remote string, hdr ...string) *http.Request {
		r := reqFrom(remote, hdr...)
		r.TLS = &tls.ConnectionState{}
		return r
	}
	for _, c := range []struct {
		name string
		r    *http.Request
		want string
	}{
		{"trusted proxy says http over a TLS backend", withTLS("10.0.0.1:1", "X-Forwarded-Proto", "http"), "http"},
		{"trusted proxy says https over a TLS backend", withTLS("10.0.0.1:1", "X-Forwarded-Proto", "https"), "https"},
		{"trusted proxy without header over a TLS backend", withTLS("10.0.0.1:1"), "https"},
		{"trusted proxy with junk header over a TLS backend", withTLS("10.0.0.1:1", "X-Forwarded-Proto", "gopher"), "https"},
		{"untrusted peer cannot downgrade a TLS connection", withTLS("203.0.113.1:1", "X-Forwarded-Proto", "http"), "https"},
		{"trusted proxy over plain HTTP", reqFrom("10.0.0.1:1", "X-Forwarded-Proto", "https"), "https"},
		{"trusted proxy over plain HTTP without header", reqFrom("10.0.0.1:1"), "http"},
	} {
		if got := n.Scheme(c.r); got != c.want {
			t.Errorf("%s: Scheme = %s, want %s", c.name, got, c.want)
		}
	}
	if got := n.BaseURL(withTLS("10.0.0.1:1", "X-Forwarded-Proto", "http")); got != "http://hub.test" {
		t.Errorf("BaseURL = %s, want http://hub.test", got)
	}

	// Over the network: the Hub terminates TLS and a trusted proxy (here the
	// loopback client) says the browser used http.
	h := startHub(t, &Config{DataDir: hubstore.Dir(t), TrustedProxies: mustPrefixes(t, "127.0.0.0/8")}, registryWith(t, false, nil))
	req, _ := http.NewRequest(http.MethodGet, "https://"+h.srv.Addr()+"/whoami", nil)
	req.Header.Set("X-Forwarded-Proto", "http")
	resp, err := insecureClient(0).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "scheme=http ") {
		t.Errorf("/whoami = %s, want scheme=http", b)
	}
}

// ---- generated certificate pair --------------------------------------------

func TestInterruptedRenewalKeepsThePreviousPair(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadCertSource(CertOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, TLSDir, TLSCertFile)
	certBefore, _ := os.ReadFile(certPath)
	keyBefore, _ := os.ReadFile(filepath.Join(dir, TLSDir, TLSKeyFile))

	// Fail the certificate rename, the second step of publishing a pair.
	realRename := renameFile
	renameFile = func(from, to string) error {
		if to == certPath {
			return errors.New("injected rename failure")
		}
		return realRename(from, to)
	}
	t.Cleanup(func() { renameFile = realRename })

	late := func() time.Time { return time.Now().Add(selfSignedValidity - renewBefore/2) } // due for renewal
	if _, err := LoadCertSource(CertOptions{DataDir: dir, Now: late}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("renewal with a failing rename = %v", err)
	}
	renameFile = realRename

	if b, _ := os.ReadFile(certPath); string(b) != string(certBefore) {
		t.Error("the certificate changed although the renewal failed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, TLSDir, TLSKeyFile)); string(b) != string(keyBefore) {
		t.Error("the key was not restored after the failed renewal")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, TLSDir))
	for _, e := range entries {
		if e.Name() != TLSCertFile && e.Name() != TLSKeyFile {
			t.Errorf("leftover file %s after a failed renewal", e.Name())
		}
	}
	again, err := LoadCertSource(CertOptions{DataDir: dir})
	if err != nil {
		t.Fatalf("start after the failed renewal: %v", err)
	}
	if again.Fingerprint() != first.Fingerprint() || again.Generated {
		t.Error("the Hub does not keep serving the previous certificate")
	}
}

func TestFirstGenerationFailureLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, TLSDir, TLSCertFile)
	realRename := renameFile
	renameFile = func(from, to string) error {
		if to == certPath {
			return errors.New("injected rename failure")
		}
		return realRename(from, to)
	}
	t.Cleanup(func() { renameFile = realRename })
	if _, err := LoadCertSource(CertOptions{DataDir: dir}); err == nil {
		t.Fatal("generation with a failing rename succeeded")
	}
	renameFile = realRename
	if _, err := LoadCertSource(CertOptions{DataDir: dir}); err != nil {
		t.Fatalf("the next start cannot recover from a failed first generation: %v", err)
	}
}

func TestConcurrentStartsShareOnePair(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	var wg sync.WaitGroup
	sources := make([]*CertSource, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sources[i], errs[i] = LoadCertSource(CertOptions{DataDir: dir})
		}()
	}
	close(start)
	wg.Wait()
	generated := 0
	for i := range sources {
		if errs[i] != nil {
			t.Fatalf("start %d: %v", i, errs[i])
		}
		if sources[i].Generated {
			generated++
		}
		if sources[i].Fingerprint() != sources[0].Fingerprint() {
			t.Errorf("start %d serves a different certificate", i)
		}
	}
	if generated != 1 {
		t.Errorf("%d starts generated a pair, want exactly one", generated)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: dir}); err != nil {
		t.Errorf("the pair on disk is unusable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, TLSDir, lockName)); err == nil {
		t.Error("the generation lock was left behind")
	}
}

func TestStaleGenerationLockIsTakenOver(t *testing.T) {
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, TLSDir)
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(tlsDir, lockName)
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStaleAfter)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: dir}); err != nil {
		t.Fatalf("a lock left by a dead process blocks the start: %v", err)
	}
}
