package version

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/tuf"
)

// stalledTUF is a TUF repository that accepts requests and never answers.
func stalledTUF(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK) // headers out, then the body stalls
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestTUFTrustedRootStopsWithTheContext: a stalled trust-root download must
// end with the update's context instead of outliving it.
func TestTUFTrustedRootStopsWithTheContext(t *testing.T) {
	srv, hits := stalledTUF(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := TUFTrustedRoot{Dir: t.TempDir(), RepositoryURL: srv.URL}.TrustedMaterial(ctx, false)
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("err = %v, want ErrNetwork", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("returned after %v, the stall outlived the context", d)
	}
	if hits.Load() == 0 {
		t.Error("the stalled repository was never asked: the test proves nothing")
	}
}

func TestTUFTrustedRootCanceledContextSendsNothing(t *testing.T) {
	srv, hits := stalledTUF(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := TUFTrustedRoot{Dir: t.TempDir(), RepositoryURL: srv.URL}.TrustedMaterial(ctx, false)
	if !errors.Is(err, ErrNetwork) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests sent with a canceled context", hits.Load())
	}
}

func TestTUFTrustedRootRefusesInsecureRepository(t *testing.T) {
	_, err := TUFTrustedRoot{Dir: t.TempDir(), RepositoryURL: "http://example.com/tuf"}.TrustedMaterial(context.Background(), false)
	if err == nil {
		t.Fatal("a plain-http remote TUF repository was accepted")
	}
}

func TestTUFTrustedRootOfflineNeedsTheCachedCopy(t *testing.T) {
	_, err := TUFTrustedRoot{Dir: t.TempDir()}.TrustedMaterial(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "while online") {
		t.Fatalf("offline without a cached root: err = %v", err)
	}
	if _, err := (TUFTrustedRoot{}).TrustedMaterial(context.Background(), true); err == nil {
		t.Error("no directory accepted")
	}
	if _, err := (StaticTrustedRoot{}).TrustedMaterial(context.Background(), false); err == nil {
		t.Error("empty static root accepted")
	}
}

const testRepoURL = "http://127.0.0.1:1/tuf"

// verifiesRecordedRelease reports whether m verifies the recorded release.
func verifiesRecordedRelease(t *testing.T, p TrustedRootProvider) error {
	t.Helper()
	m, err := p.TrustedMaterial(context.Background(), true)
	if err != nil {
		return err
	}
	return NewSigstoreVerifier(m).VerifyBlob(readTestdata(t, "checksums.txt"), readTestdata(t, "checksums.valid.sigstore.json"), recordedTag)
}

// TestTUFTrustedRootOnlineThenOffline: an online fetch (through a root
// rotation) leaves a cache that authenticates offline, with no network.
func TestTUFTrustedRootOnlineThenOffline(t *testing.T) {
	repo := newTestTUF(t, readTestdata(t, "trusted_root.json"), true, time.Now().Add(time.Hour))
	srv := repo.serve(t)
	dir := t.TempDir()
	p := TUFTrustedRoot{Dir: dir, RepositoryURL: srv.URL, Root: repo.anchor}
	if _, err := p.TrustedMaterial(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if err := verifiesRecordedRelease(t, p); err != nil {
		t.Fatal(err)
	}
}

// TestTUFTrustedRootOfflineAcceptsExpiredButSignedMetadata: a cache that has
// been offline for longer than the TUF expiry still verifies, because offline
// cannot refresh.
func TestTUFTrustedRootOfflineAcceptsExpiredButSignedMetadata(t *testing.T) {
	repo := newTestTUF(t, readTestdata(t, "trusted_root.json"), true, time.Now().Add(-30*24*time.Hour))
	dir := t.TempDir()
	repo.install(t, dir, testRepoURL)
	if err := verifiesRecordedRelease(t, TUFTrustedRoot{Dir: dir, RepositoryURL: testRepoURL, Root: repo.anchor}); err != nil {
		t.Fatal(err)
	}
}

// TestTUFTrustedRootOfflineRejectsUnauthenticatedCache: whoever can write the
// cache must not be able to substitute the trusted root.
func TestTUFTrustedRootOfflineRejectsUnauthenticatedCache(t *testing.T) {
	legit := readTestdata(t, "trusted_root.json")
	later := time.Now().Add(time.Hour)
	attacks := map[string]func(t *testing.T, dir string, repo *testTUF){
		"trusted root file only": func(t *testing.T, dir string, _ *testTUF) {
			writeFile(t, filepath.Join(dir, trustedRootFile), strings.Replace(string(legit), "rekor", "rekoR", 1), 0o600)
		},
		"no TUF metadata at all": func(t *testing.T, dir string, _ *testTUF) {
			if err := os.RemoveAll(filepath.Join(dir, "tuf")); err != nil {
				t.Fatal(err)
			}
		},
		"attacker repository with its own keys": func(t *testing.T, dir string, _ *testTUF) {
			evil := newTestTUF(t, []byte(`{"mediaType":"evil"}`), false, later)
			evil.install(t, dir, testRepoURL)
		},
		"attacker repository with a forged root chain": func(t *testing.T, dir string, _ *testTUF) {
			evil := newTestTUF(t, []byte(`{"mediaType":"evil"}`), true, later)
			evil.install(t, dir, testRepoURL)
		},
		"forged root chain over legitimate metadata": func(t *testing.T, dir string, _ *testTUF) {
			evil := newTestTUF(t, legit, true, later)
			writeFile(t, filepath.Join(dir, tufRootsDir, "2.root.json"), string(evil.roots[2]), 0o600)
		},
		"tampered targets metadata": func(t *testing.T, dir string, _ *testTUF) {
			p := filepath.Join(dir, "tuf", tuf.URLToPath(testRepoURL), "targets.json")
			writeFile(t, p, strings.Replace(readFile(t, p), "trusted_root.json", "trusted_root.jsoN", 1), 0o600)
		},
		"stale timestamp for other snapshot": func(t *testing.T, dir string, _ *testTUF) {
			other := newTestTUF(t, legit, false, later)
			writeFile(t, filepath.Join(dir, "tuf", tuf.URLToPath(testRepoURL), "timestamp.json"), string(other.timestamp), 0o600)
		},
	}
	for name, attack := range attacks {
		t.Run(name, func(t *testing.T) {
			repo := newTestTUF(t, legit, true, later)
			dir := t.TempDir()
			repo.install(t, dir, testRepoURL)
			p := TUFTrustedRoot{Dir: dir, RepositoryURL: testRepoURL, Root: repo.anchor}
			if err := verifiesRecordedRelease(t, p); err != nil {
				t.Fatalf("the untouched cache must work: %v", err)
			}
			attack(t, dir, repo)
			_, err := p.TrustedMaterial(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), "while online") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestTUFTrustedRootOfflineUsesTheEmbeddedAnchor: with no anchor override the
// embedded public-good root decides, so a private repository's cache is not
// accepted.
func TestTUFTrustedRootOfflineUsesTheEmbeddedAnchor(t *testing.T) {
	repo := newTestTUF(t, readTestdata(t, "trusted_root.json"), false, time.Now().Add(time.Hour))
	dir := t.TempDir()
	repo.install(t, dir, "")
	if _, err := (TUFTrustedRoot{Dir: dir}).TrustedMaterial(context.Background(), true); err == nil {
		t.Fatal("a cache signed by foreign keys passed the embedded anchor")
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestUpdateOfflineRejectsSubstitutedTrustRootAndRelease is the end-to-end
// attack on the offline path: the attacker replaces the cached trusted root
// (and the Sigstore roots in it) together with a release signed under their
// own Fulcio and Rekor, archive included. The production loader must not
// install it.
func TestUpdateOfflineRejectsSubstitutedTrustRootAndRelease(t *testing.T) {
	legitHost := newFakeHost(t, fakeRelease{tag: "v1.2.3"})
	evilHost := newFakeHost(t, fakeRelease{tag: "v1.2.3", binary: "evil"})
	later := time.Now().Add(time.Hour)
	legitTUF := newTestTUF(t, legitHost.sig.trustedRootJSON(t), false, later)
	evilTUF := newTestTUF(t, evilHost.sig.trustedRootJSON(t), false, later)
	tufSrv := legitTUF.serve(t)

	// The attacker builds a complete cache offline with their own material.
	evilEnv := newEnv(t, evilHost)
	if _, err := evilEnv.run(evilEnv.opts("v1.2.3", "v0.1.0")); err != nil {
		t.Fatal(err)
	}

	// The victim's production-style provider, filled by a legitimate online run.
	e := newEnv(t, legitHost)
	prod := TUFTrustedRoot{Dir: e.cache, RepositoryURL: tufSrv.URL, Root: legitTUF.anchor}
	e.updater.Verifier = SigstoreVerifierFactory(prod)
	fillCache(t, e, "v1.2.3")

	// Positive control: the legitimate cache, filled online by the production
	// provider from a pretty-printed (not protojson-canonical) target,
	// installs offline with that same provider. Without it the rejection
	// below would also pass if offline verification were always broken.
	goOffline(e)
	oc := e.opts("1.2", "v1.0.0")
	oc.Offline = true
	if _, err := e.run(oc); err != nil {
		t.Fatalf("positive control: the legitimate online-filled cache is refused offline: %v", err)
	}
	if got := e.installed(); got == "installed binary" || got == "evil" {
		t.Fatalf("positive control installed %q", got)
	}
	writeFile(t, e.exec, "installed binary", 0o755)

	// The attacker overwrites the whole cache directory.
	if err := os.RemoveAll(e.cache); err != nil {
		t.Fatal(err)
	}
	copyTree(t, evilEnv.cache, e.cache)
	evilTUF.install(t, e.cache, tufSrv.URL)
	goOffline(e)
	o := e.opts("1.2", "v1.0.0")
	o.Offline = true

	// Control: with the attacker's material trusted the cache is consistent,
	// so the test proves that only the trust root authentication stops it.
	e.updater.Verifier = SigstoreVerifierFactory(StaticTrustedRoot{Material: evilHost.sig.material(t)})
	if _, err := e.run(o); err != nil {
		t.Fatalf("control: the forged cache is not self-consistent: %v", err)
	}
	if e.installed() != "evil" {
		t.Fatalf("control installed %q", e.installed())
	}
	writeFile(t, e.exec, "installed binary", 0o755)

	e.updater.Verifier = SigstoreVerifierFactory(prod)
	if _, err := e.run(o); err == nil {
		t.Fatal("the substituted trust root and release were accepted")
	}
	if e.installed() != "installed binary" {
		t.Errorf("installed %q", e.installed())
	}
}
