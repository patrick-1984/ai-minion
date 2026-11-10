package version

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testOS   = "linux"
	testArch = "amd64"
)

// fakeRelease describes one release the fake host serves.
type fakeRelease struct {
	tag        string
	prerelease bool
	draft      bool
	protoMin   string // default "1.0"
	protoMax   string // default "1.1"
	binary     string // content of the minion binary, default "minion <tag>"
}

// fakeHost is a local release server speaking the GitHub releases API, with
// assets signed by a private Sigstore. tamper changes an asset on the way out.
type fakeHost struct {
	t        *testing.T
	sig      *fakeSigstore
	srv      *httptest.Server
	releases []fakeRelease

	mu       sync.Mutex
	requests []string                                        // request paths in order
	tamper   map[string]func(tag string, data []byte) []byte // by asset name
}

func newFakeHost(t *testing.T, releases ...fakeRelease) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, sig: newFakeSigstore(t), releases: releases, tamper: map[string]func(string, []byte) []byte{}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (r fakeRelease) withDefaults() fakeRelease {
	if r.protoMin == "" {
		r.protoMin = "1.0"
	}
	if r.protoMax == "" {
		r.protoMax = "1.1"
	}
	if r.binary == "" {
		r.binary = "minion " + r.tag
	}
	return r
}

// assets builds every asset of a release, signed at the current time.
func (h *fakeHost) assets(r fakeRelease) map[string][]byte {
	r = r.withDefaults()
	archive := ArchiveName(r.tag, testOS, testArch, "")
	archiveData := makeTarGz(h.t, archiveFile{name: "LICENSE", data: []byte("license")}, archiveFile{name: "minion", data: []byte(r.binary)})
	sum := sha256.Sum256(archiveData)
	archiveSum := hex.EncodeToString(sum[:])
	other := ArchiveName(r.tag, "darwin", "arm64", "")
	checksums := []byte(archiveSum + "  " + archive + "\n" + strings.Repeat("ab", 32) + "  " + other + "\n")
	manifest := manifestJSON(r.tag, r.protoMin, r.protoMax,
		`{"name":"`+archive+`","sha256":"`+archiveSum+`"},{"name":"`+other+`","sha256":"`+strings.Repeat("ab", 32)+`"}`)
	spec := releaseSpec(r.tag, time.Now())
	return map[string][]byte{
		archive:                    archiveData,
		ChecksumsAsset:             checksums,
		BundleName(ChecksumsAsset): h.sig.sign(h.t, checksums, spec),
		ManifestAsset:              manifest,
		BundleName(ManifestAsset):  h.sig.sign(h.t, manifest, spec),
	}
}

func (h *fakeHost) serve(w http.ResponseWriter, req *http.Request) {
	h.mu.Lock()
	h.requests = append(h.requests, req.URL.Path)
	h.mu.Unlock()
	switch {
	case req.URL.Path == "/repos/"+RepoOwner+"/"+RepoName+"/releases":
		var out []ghRelease
		for _, r := range h.releases {
			gr := ghRelease{TagName: r.tag, Draft: r.draft, Prerelease: r.prerelease}
			if !r.draft {
				for name, data := range h.assets(r) {
					gr.Assets = append(gr.Assets, ghAsset{Name: name, URL: h.srv.URL + "/dl/" + r.tag + "/" + name, Size: int64(len(data))})
				}
			}
			out = append(out, gr)
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(req.URL.Path, "/dl/"):
		parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/dl/"), "/", 2)
		for _, r := range h.releases {
			if r.tag != parts[0] {
				continue
			}
			data, ok := h.assets(r)[parts[1]]
			if !ok {
				break
			}
			h.mu.Lock()
			mut := h.tamper[parts[1]]
			h.mu.Unlock()
			if mut != nil {
				data = mut(r.tag, data)
			}
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (h *fakeHost) setTamper(asset string, f func(tag string, data []byte) []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tamper[asset] = f
}

func (h *fakeHost) archiveRequests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.requests {
		if strings.HasSuffix(p, ".tar.gz") {
			n++
		}
	}
	return n
}

// env bundles an updater, its installed binary and options.
type env struct {
	t       *testing.T
	host    *fakeHost
	updater *Updater
	exec    string
	cache   string
	out     *strings.Builder
}

func newEnv(t *testing.T, host *fakeHost) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, host: host, exec: filepath.Join(dir, "minion"), cache: filepath.Join(dir, "cache"), out: &strings.Builder{}}
	writeFile(t, e.exec, "installed binary", 0o755)
	src, err := NewGitHubSource(host.srv.URL, "minion-test")
	if err != nil {
		t.Fatal(err)
	}
	e.updater = &Updater{
		Source:   src,
		Verifier: SigstoreVerifierFactory(StaticTrustedRoot{Material: host.sig.material(t)}),
		Cache:    Cache{Dir: e.cache},
		Out:      e.out,
	}
	return e
}

func (e *env) opts(selector, current string) Options {
	sel, err := ParseSelector(selector)
	if err != nil {
		e.t.Fatal(err)
	}
	return Options{Selector: sel, Current: current, ExecPath: e.exec, GOOS: testOS, GOARCH: testArch}
}

func (e *env) run(o Options) (Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.updater.Run(ctx, o)
}

func (e *env) installed() string { return readFile(e.t, e.exec) }

func standardHost(t *testing.T) *fakeHost {
	return newFakeHost(t,
		fakeRelease{tag: "v1.2.3"},
		fakeRelease{tag: "v1.2.4"},
		fakeRelease{tag: "v1.3.0"},
		fakeRelease{tag: "v1.4.0-rc.1", prerelease: true},
		fakeRelease{tag: "v1.3.1", draft: true},
		fakeRelease{tag: "v0.9.0"},
	)
}

func TestUpdateInstallsExactVersion(t *testing.T) {
	e := newEnv(t, standardHost(t))
	res, err := e.run(e.opts("v1.2.3", "v1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Tag != "v1.2.3" || res.UpToDate || res.FromCache || res.Previous != "v1.0.0" || res.Path != e.exec {
		t.Errorf("result = %+v", res)
	}
	if got := e.installed(); got != "minion v1.2.3" {
		t.Errorf("installed binary = %q", got)
	}
	if !strings.Contains(e.out.String(), "downloading minion_1.2.3_linux_amd64.tar.gz") {
		t.Errorf("progress = %q", e.out.String())
	}
	// The verified release is cached with its signed metadata.
	for _, name := range []string{"minion", "minion_1.2.3_linux_amd64.tar.gz", "meta.json", "checksums.txt", "checksums.txt.sigstore.json", "release-manifest.json", "release-manifest.json.sigstore.json"} {
		if _, err := os.Stat(filepath.Join(e.cache, "v1.2.3", name)); err != nil {
			t.Errorf("cache entry lacks %s: %v", name, err)
		}
	}
}

func TestUpdateChannelsPickNewestNonPrerelease(t *testing.T) {
	cases := map[string]string{
		"latest": "v1.3.0", // not the rc v1.4.0-rc.1, not the draft v1.3.1
		"1":      "v1.3.0",
		"1.2":    "v1.2.4",
		"0.9":    "v0.9.0",
	}
	for sel, want := range cases {
		t.Run(sel, func(t *testing.T) {
			e := newEnv(t, standardHost(t))
			res, err := e.run(Options{Selector: mustSelector(t, sel), Current: "v0.1.0", ExecPath: e.exec, GOOS: testOS, GOARCH: testArch})
			if err != nil {
				t.Fatal(err)
			}
			if res.Tag != want || e.installed() != "minion "+want {
				t.Errorf("tag = %s, installed %q; want %s", res.Tag, e.installed(), want)
			}
		})
	}
}

func mustSelector(t *testing.T, s string) Selector {
	t.Helper()
	sel, err := ParseSelector(s)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func TestUpdateRCNeverSatisfiesChannelButExactRCWorks(t *testing.T) {
	host := newFakeHost(t, fakeRelease{tag: "v1.4.0-rc.1", prerelease: true}, fakeRelease{tag: "v1.3.0"})
	e := newEnv(t, host)
	if res, err := e.run(e.opts("1.4", "v1.0.0")); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("channel 1.4 = %+v, %v; want ErrNoRelease", res, err)
	}
	if e.installed() != "installed binary" {
		t.Error("binary changed")
	}
	if res, err := e.run(e.opts("v1.4.0-rc.1", "v1.0.0")); err != nil || res.Tag != "v1.4.0-rc.1" {
		t.Fatalf("exact rc = %+v, %v", res, err)
	}
}

func TestUpdateDowngrade(t *testing.T) {
	e := newEnv(t, standardHost(t))
	_, err := e.run(e.opts("1.2", "v1.3.0"))
	if !errors.Is(err, ErrDowngrade) || !strings.Contains(err.Error(), "--allow-downgrade") {
		t.Fatalf("err = %v, want ErrDowngrade naming --allow-downgrade", err)
	}
	if e.installed() != "installed binary" || e.host.archiveRequests() != 0 {
		t.Error("a refused downgrade must not download or install anything")
	}

	o := e.opts("1.2", "v1.3.0")
	o.AllowDowngrade = true
	res, err := e.run(o)
	if err != nil || res.Tag != "v1.2.4" || e.installed() != "minion v1.2.4" {
		t.Fatalf("allowed downgrade: %+v, %v, installed %q", res, err, e.installed())
	}
}

func TestUpdateUpToDateAndDevBuild(t *testing.T) {
	e := newEnv(t, standardHost(t))
	res, err := e.run(e.opts("v1.2.3", "v1.2.3"))
	if err != nil || !res.UpToDate || e.installed() != "installed binary" || e.host.archiveRequests() != 0 {
		t.Fatalf("same version: %+v, %v", res, err)
	}
	// A "dev" build has nothing to compare with: even an older release installs.
	res, err = e.run(e.opts("v0.9.0", "dev"))
	if err != nil || res.UpToDate || e.installed() != "minion v0.9.0" {
		t.Fatalf("dev build: %+v, %v, installed %q", res, err, e.installed())
	}
}

func TestUpdateProtocolOverlap(t *testing.T) {
	host := newFakeHost(t,
		fakeRelease{tag: "v1.5.0", protoMin: "1.1", protoMax: "1.2"},
		fakeRelease{tag: "v1.6.0", protoMin: "1.3", protoMax: "1.4"},
		fakeRelease{tag: "v2.0.0", protoMin: "2.0", protoMax: "2.0"},
	)
	hub := func(s string) *ProtoRange { r := mustRange(t, s); return &r }
	cases := []struct {
		name    string
		sel     string
		hub     *ProtoRange
		wantErr bool
	}{
		{"no Hub connection: nothing to check", "v1.6.0", nil, false},
		{"same major, ranges overlap", "v1.5.0", hub("1.0..1.1"), false},
		{"same major, new range inside the Hub window", "v1.5.0", hub("1.0..1.3"), false},
		{"same major, disjoint minor ranges", "v1.6.0", hub("1.0..1.1"), true},
		{"same major, adjacent but disjoint", "v1.6.0", hub("1.1..1.2"), true},
		{"different major", "v2.0.0", hub("1.0..1.1"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, host)
			o := e.opts(tc.sel, "v1.0.0")
			o.HubRange = tc.hub
			before := host.archiveRequests()
			_, err := e.run(o)
			if !tc.wantErr {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrIncompatible) || !strings.Contains(err.Error(), "incompatible with the running Hub connection") {
				t.Fatalf("err = %v, want the A-10 refusal", err)
			}
			if e.installed() != "installed binary" {
				t.Error("binary changed")
			}
			if host.archiveRequests() != before {
				t.Error("the archive was downloaded before the protocol check")
			}
			if _, serr := os.Stat(filepath.Join(e.cache, tc.sel)); serr == nil {
				t.Error("a refused release was cached")
			}
		})
	}
}

func TestUpdateRejectsTamperedArchive(t *testing.T) {
	e := newEnv(t, standardHost(t))
	e.host.setTamper(ArchiveName("v1.2.3", testOS, testArch, ""), func(_ string, data []byte) []byte {
		return append(append([]byte(nil), data...), 0)
	})
	_, err := e.run(e.opts("v1.2.3", "v1.0.0"))
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if e.installed() != "installed binary" {
		t.Error("binary changed")
	}
	assertNoCacheEntry(t, e, "v1.2.3")
}

func assertNoCacheEntry(t *testing.T, e *env, tag string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(e.cache, tag)); err == nil {
		t.Errorf("cache entry %s exists", tag)
	}
	ents, _ := os.ReadDir(e.cache)
	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), ".stage-") {
			t.Errorf("staging directory %s left behind", ent.Name())
		}
	}
}

func TestUpdateRejectsBadSignatures(t *testing.T) {
	otherSigstore := newFakeSigstore(t)
	cases := map[string]struct {
		asset  string
		tamper func(tag string, data []byte) []byte
	}{
		"checksums changed after signing": {ChecksumsAsset, func(_ string, d []byte) []byte {
			return []byte(strings.Replace(string(d), "  ", " x", 1))
		}},
		"manifest changed after signing": {ManifestAsset, func(_ string, d []byte) []byte {
			return []byte(strings.Replace(string(d), `"max":"1.1"`, `"max":"1.9"`, 1))
		}},
		"checksums bundle from another tag": {BundleName(ChecksumsAsset), func(_ string, d []byte) []byte { return d }}, // replaced below
		"manifest bundle signed by an unknown CA": {BundleName(ManifestAsset), func(tag string, _ []byte) []byte {
			return otherSigstore.sign(t, []byte("whatever"), releaseSpec(tag, time.Now()))
		}},
		"checksums bundle is garbage": {BundleName(ChecksumsAsset), func(string, []byte) []byte { return []byte("{}") }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, standardHost(t))
			tamper := tc.tamper
			if name == "checksums bundle from another tag" {
				tamper = func(_ string, _ []byte) []byte {
					cs := e.host.assets(fakeRelease{tag: "v1.2.4"})
					return cs[BundleName(ChecksumsAsset)]
				}
			}
			e.host.setTamper(tc.asset, tamper)
			_, err := e.run(e.opts("v1.2.3", "v1.0.0"))
			if !errors.Is(err, ErrVerification) {
				t.Fatalf("err = %v, want ErrVerification", err)
			}
			if e.installed() != "installed binary" || e.host.archiveRequests() != 0 {
				t.Error("an unverified release must not be downloaded or installed")
			}
			assertNoCacheEntry(t, e, "v1.2.3")
		})
	}
}

func TestUpdateManifestAndChecksumsMustAgree(t *testing.T) {
	// Both files are signed, but they disagree about the archive: refuse.
	host := newFakeHost(t, fakeRelease{tag: "v1.2.3"})
	e := newEnv(t, host)
	archive := ArchiveName("v1.2.3", testOS, testArch, "")
	assets := host.assets(fakeRelease{tag: "v1.2.3"})
	badManifest := manifestJSON("v1.2.3", "1.0", "1.1", `{"name":"`+archive+`","sha256":"`+strings.Repeat("cd", 32)+`"}`)
	host.setTamper(ManifestAsset, func(string, []byte) []byte { return badManifest })
	host.setTamper(BundleName(ManifestAsset), func(tag string, _ []byte) []byte {
		return host.sig.sign(t, badManifest, releaseSpec(tag, time.Now()))
	})
	_ = assets
	_, err := e.run(e.opts("v1.2.3", "v1.0.0"))
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if e.installed() != "installed binary" {
		t.Error("binary changed")
	}
}

func TestUpdateMissingAssets(t *testing.T) {
	host := newFakeHost(t, fakeRelease{tag: "v1.2.3"})
	e := newEnv(t, host)
	// A platform without an archive in the release.
	o := e.opts("v1.2.3", "v1.0.0")
	o.GOARCH = "riscv64"
	if _, err := e.run(o); err == nil || !strings.Contains(err.Error(), "no archive for linux/riscv64") {
		t.Fatalf("err = %v", err)
	}
	// A release without a signature bundle cannot be verified.
	e.updater.Source = dropAssets{e.updater.Source, BundleName(ChecksumsAsset)}
	if _, err := e.run(e.opts("v1.2.3", "v1.0.0")); err == nil || !strings.Contains(err.Error(), "cannot be verified") {
		t.Fatalf("err = %v", err)
	}
	if e.installed() != "installed binary" {
		t.Error("binary changed")
	}
}

// dropAssets hides named assets of every release.
type dropAssets struct {
	Source
	names string
}

func (d dropAssets) Releases(ctx context.Context) ([]Release, error) {
	rs, err := d.Source.Releases(ctx)
	for i := range rs {
		var keep []Asset
		for _, a := range rs[i].Assets {
			if a.Name != d.names {
				keep = append(keep, a)
			}
		}
		rs[i].Assets = keep
	}
	return rs, err
}

func TestUpdateNetworkDownHintsOffline(t *testing.T) {
	host := standardHost(t)
	e := newEnv(t, host)
	host.srv.Close()
	_, err := e.run(e.opts("latest", "v1.0.0"))
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("err = %v, want ErrNetwork", err)
	}
	if !strings.Contains(err.Error(), "--offline") {
		t.Errorf("error %q does not point to --offline", err)
	}
	if e.installed() != "installed binary" {
		t.Error("binary changed")
	}
}

func TestUpdateHTTPErrorIsNotANetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "rate limited", http.StatusForbidden) }))
	defer srv.Close()
	src, err := NewGitHubSource(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Releases(context.Background())
	if err == nil || errors.Is(err, ErrNetwork) || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("err = %v", err)
	}
}

// ---- offline --------------------------------------------------------------

// failingSource fails the test when it is used at all.
type failingSource struct{ t *testing.T }

func (f failingSource) Releases(context.Context) ([]Release, error) {
	f.t.Error("offline run listed releases")
	return nil, errors.New("network used")
}
func (f failingSource) Open(context.Context, Asset) (io.ReadCloser, error) {
	f.t.Error("offline run downloaded an asset")
	return nil, errors.New("network used")
}

func fillCache(t *testing.T, e *env, tags ...string) {
	t.Helper()
	for _, tag := range tags {
		if _, err := e.run(e.opts(tag, "v0.1.0")); err != nil {
			t.Fatalf("online install %s: %v", tag, err)
		}
	}
	writeFile(t, e.exec, "installed binary", 0o755)
}

func goOffline(e *env) {
	e.host.srv.Close()
	e.updater.Source = failingSource{e.t}
}

func TestUpdateOfflineUsesNewestVerifiedCachedMatch(t *testing.T) {
	e := newEnv(t, standardHost(t))
	fillCache(t, e, "v1.2.3", "v1.2.4", "v1.3.0", "v0.9.0")
	goOffline(e)

	cases := []struct{ sel, current, want string }{
		{"1.2", "v1.0.0", "v1.2.4"},
		{"latest", "v1.0.0", "v1.3.0"},
		{"1", "v1.0.0", "v1.3.0"},
		{"v1.2.3", "v1.0.0", "v1.2.3"},
		{"0", "v1.0.0", "v0.9.0"}, // an older version is a downgrade here
	}
	for _, tc := range cases {
		t.Run(tc.sel, func(t *testing.T) {
			writeFile(t, e.exec, "installed binary", 0o755)
			o := e.opts(tc.sel, tc.current)
			o.Offline = true
			o.AllowDowngrade = tc.sel == "0"
			res, err := e.run(o)
			if err != nil {
				t.Fatal(err)
			}
			if res.Tag != tc.want || !res.FromCache || e.installed() != "minion "+tc.want {
				t.Errorf("result %+v, installed %q; want %s", res, e.installed(), tc.want)
			}
		})
	}
}

func TestUpdateOfflineNoMatch(t *testing.T) {
	e := newEnv(t, standardHost(t))
	fillCache(t, e, "v1.2.3")
	goOffline(e)
	for _, sel := range []string{"1.3", "2", "v1.2.4"} {
		o := e.opts(sel, "v1.0.0")
		o.Offline = true
		_, err := e.run(o)
		if !errors.Is(err, ErrNoCached) || !strings.Contains(err.Error(), "while online") {
			t.Errorf("selector %s: err = %v, want a clear ErrNoCached", sel, err)
		}
	}
	if e.installed() != "installed binary" {
		t.Error("binary changed")
	}
}

func TestUpdateOfflineEmptyCache(t *testing.T) {
	e := newEnv(t, standardHost(t))
	goOffline(e)
	o := e.opts("latest", "v1.0.0")
	o.Offline = true
	if _, err := e.run(o); !errors.Is(err, ErrNoCached) {
		t.Fatalf("err = %v, want ErrNoCached", err)
	}
}

func TestUpdateOfflineSkipsDamagedEntries(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"binary modified": func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "minion"), "evil", 0o755) },
		"binary removed":  func(t *testing.T, dir string) { _ = os.Remove(filepath.Join(dir, "minion")) },
		"archive removed": func(t *testing.T, dir string) { removeArchive(t, dir) },
		"archive modified": func(t *testing.T, dir string) {
			writeFile(t, archivePath(t, dir), "not the signed archive", 0o600)
		},
		"checksums edited": func(t *testing.T, dir string) {
			p := filepath.Join(dir, "checksums.txt")
			writeFile(t, p, strings.Replace(readFile(t, p), "  ", " x", 1), 0o600)
		},
		"bundle removed": func(t *testing.T, dir string) { _ = os.Remove(filepath.Join(dir, "checksums.txt.sigstore.json")) },
		"meta removed":   func(t *testing.T, dir string) { _ = os.Remove(filepath.Join(dir, "meta.json")) },
		"meta for another platform": func(t *testing.T, dir string) {
			p := filepath.Join(dir, "meta.json")
			writeFile(t, p, strings.Replace(readFile(t, p), `"goarch": "amd64"`, `"goarch": "arm64"`, 1), 0o600)
		},
		"recorded digest changed": func(t *testing.T, dir string) {
			m, _ := readMeta(dir)
			m.BinarySHA256 = strings.Repeat("0", 64)
			_ = writeMeta(dir, m)
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, standardHost(t))
			fillCache(t, e, "v1.2.3", "v1.2.4")
			damage(t, e.updater.Cache.EntryDir("v1.2.4"))
			goOffline(e)
			o := e.opts("1.2", "v1.0.0")
			o.Offline = true
			res, err := e.run(o)
			if err != nil || res.Tag != "v1.2.3" || e.installed() != "minion v1.2.3" {
				t.Fatalf("fell back to %+v, %v, installed %q; want v1.2.3", res, err, e.installed())
			}
			// And with the only entry damaged, the error names the problem.
			damage(t, e.updater.Cache.EntryDir("v1.2.3"))
			writeFile(t, e.exec, "installed binary", 0o755)
			if _, err := e.run(o); !errors.Is(err, ErrNoCached) || !strings.Contains(err.Error(), "unusable cache entries") {
				t.Fatalf("err = %v", err)
			}
			if e.installed() != "installed binary" {
				t.Error("a damaged cache entry was installed")
			}
		})
	}
}

func TestUpdateOfflineKeepsDowngradeAndProtocolRules(t *testing.T) {
	host := newFakeHost(t,
		fakeRelease{tag: "v1.2.0", protoMin: "1.0", protoMax: "1.1"},
		fakeRelease{tag: "v1.3.0", protoMin: "1.3", protoMax: "1.4"},
	)
	e := newEnv(t, host)
	fillCache(t, e, "v1.2.0", "v1.3.0")
	goOffline(e)

	o := e.opts("1.2", "v1.3.0")
	o.Offline = true
	if _, err := e.run(o); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("offline downgrade: err = %v", err)
	}
	r := mustRange(t, "1.0..1.1")
	o = e.opts("1.3", "v1.2.0")
	o.Offline = true
	o.HubRange = &r
	if _, err := e.run(o); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("offline protocol: err = %v", err)
	}
	o.Selector, o.HubRange = mustSelector(t, "v1.3.0"), nil
	o.Current = "v1.3.0"
	if res, err := e.run(o); err != nil || !res.UpToDate {
		t.Fatalf("offline up to date: %+v, %v", res, err)
	}
	if e.installed() != "installed binary" {
		t.Error("binary changed")
	}
}

func TestUpdateOfflineNeverTouchesTheSource(t *testing.T) {
	e := newEnv(t, standardHost(t))
	fillCache(t, e, "v1.2.3")
	e.updater.Source = nil // an offline run must not even need one
	o := e.opts("1.2", "v1.0.0")
	o.Offline = true
	if res, err := e.run(o); err != nil || res.Tag != "v1.2.3" {
		t.Fatalf("%+v, %v", res, err)
	}
}

func TestUpdaterNeedsConfiguration(t *testing.T) {
	u := &Updater{}
	if _, err := u.Run(context.Background(), Options{ExecPath: "x"}); err == nil {
		t.Error("an updater without verifier ran")
	}
	u.Verifier = func(context.Context, bool) (BlobVerifier, error) { return nil, errors.New("no") }
	if _, err := u.Run(context.Background(), Options{ExecPath: "x"}); err == nil {
		t.Error("an online updater without source ran")
	}
}

func TestUpdateVerifierFactoryErrorStopsTheRun(t *testing.T) {
	e := newEnv(t, standardHost(t))
	e.updater.Verifier = func(context.Context, bool) (BlobVerifier, error) {
		return nil, fmt.Errorf("%w: TUF is down", ErrNetwork)
	}
	_, err := e.run(e.opts("latest", "v1.0.0"))
	if !errors.Is(err, ErrNetwork) || !strings.Contains(err.Error(), "--offline") {
		t.Fatalf("err = %v", err)
	}
	if e.host.archiveRequests() != 0 {
		t.Error("downloaded without a verifier")
	}
}

// ---- GitHub adapter -------------------------------------------------------

func TestGitHubSourcePaginates(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") != "ua-test" {
			t.Errorf("headers = %v", r.Header)
		}
		n := ghPageSize
		if r.URL.Query().Get("page") == "3" {
			n = 7
		}
		var out []ghRelease
		for i := 0; i < n; i++ {
			out = append(out, ghRelease{TagName: fmt.Sprintf("v1.%s.%d", r.URL.Query().Get("page"), i)})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	src, err := NewGitHubSource(srv.URL+"/", "ua-test")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := src.Releases(context.Background())
	if err != nil || len(rs) != 2*ghPageSize+7 || strings.Join(pages, ",") != "1,2,3" {
		t.Fatalf("got %d releases, pages %v, err %v", len(rs), pages, err)
	}
}

func TestCheckURL(t *testing.T) {
	for _, ok := range []string{"https://api.github.com", "https://example.com/x", "http://127.0.0.1:8080/x", "http://localhost:9/x", "http://[::1]:80/"} {
		if err := checkURL(ok); err != nil {
			t.Errorf("checkURL(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "http://example.com/x", "http://127.0.0.1.example.com/", "ftp://example.com", "file:///etc/passwd", "//example.com", "https://", "javascript:alert(1)"} {
		if err := checkURL(bad); err == nil {
			t.Errorf("checkURL(%q) succeeded", bad)
		}
	}
	if _, err := NewGitHubSource("http://example.com", ""); err == nil {
		t.Error("a plain-http remote API URL was accepted")
	}
	if src, err := NewGitHubSource("", ""); err != nil || src.apiURL != DefaultAPIURL {
		t.Errorf("default API URL: %v %v", src, err)
	}
}

func TestOpenRefusesInsecureAssetsAndRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/evil", http.StatusFound)
	}))
	defer srv.Close()
	src, err := NewGitHubSource(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Open(context.Background(), Asset{Name: "a", URL: "http://example.com/a"}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("remote http asset: err = %v", err)
	}
	if _, err := src.Open(context.Background(), Asset{Name: "a", URL: srv.URL + "/a"}); err == nil {
		t.Error("an https-to-http redirect to a remote host was followed")
	}
}

func archivePath(t *testing.T, dir string) string {
	t.Helper()
	m, err := readMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, m.ArchiveName)
}

func removeArchive(t *testing.T, dir string) {
	t.Helper()
	if err := os.Remove(archivePath(t, dir)); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// TestUpdateOfflineRejectsSubstitutedExecutable covers an attacker who can
// write the cache: meta.json is not signed, so changing the cached binary
// together with the digest recorded for it, or the archive together with its
// recorded digests, must not install anything. Only the signed checksums
// authenticate the executable.
func TestUpdateOfflineRejectsSubstitutedExecutable(t *testing.T) {
	attacks := map[string]func(t *testing.T, dir string){
		"binary and recorded binary digest": func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "minion"), "evil", 0o755)
			m, _ := readMeta(dir)
			m.BinarySHA256 = sha256Hex("evil")
			if err := writeMeta(dir, m); err != nil {
				t.Fatal(err)
			}
		},
		"archive, binary and every recorded digest": func(t *testing.T, dir string) {
			evil := makeTarGz(t, archiveFile{name: "minion", data: []byte("evil")})
			m, _ := readMeta(dir)
			writeFile(t, filepath.Join(dir, m.ArchiveName), string(evil), 0o600)
			writeFile(t, filepath.Join(dir, "minion"), "evil", 0o755)
			m.ArchiveSHA256 = sha256Hex(string(evil))
			m.BinarySHA256 = sha256Hex("evil")
			if err := writeMeta(dir, m); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, attack := range attacks {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, standardHost(t))
			fillCache(t, e, "v1.2.3")
			attack(t, e.updater.Cache.EntryDir("v1.2.3"))
			goOffline(e)
			o := e.opts("1.2", "v1.0.0")
			o.Offline = true
			_, err := e.run(o)
			if !errors.Is(err, ErrNoCached) || !strings.Contains(err.Error(), "unusable cache entries") {
				t.Fatalf("err = %v", err)
			}
			if e.installed() != "installed binary" {
				t.Errorf("installed %q", e.installed())
			}
		})
	}
}

// TestUpdateOfflineInstallsTheAuthenticatedArchiveBytes shows that the
// installed executable comes from the signed archive.
func TestUpdateOfflineInstallsTheAuthenticatedArchiveBytes(t *testing.T) {
	e := newEnv(t, standardHost(t))
	fillCache(t, e, "v1.2.3")
	goOffline(e)
	o := e.opts("1.2", "v1.0.0")
	o.Offline = true
	if _, err := e.run(o); err != nil {
		t.Fatal(err)
	}
	if got := e.installed(); got != "minion v1.2.3" {
		t.Errorf("installed %q", got)
	}
	ents, _ := os.ReadDir(e.cache)
	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), ".stage-") {
			t.Errorf("scratch directory %s left behind", ent.Name())
		}
	}
}

// TestUpdateOfflineChannelsHonourTheHostPrereleaseFlag: a release the host
// flagged as pre-release never satisfies a channel offline, as online, even
// when its tag looks stable; an exact selector still installs it.
func TestUpdateOfflineChannelsHonourTheHostPrereleaseFlag(t *testing.T) {
	host := newFakeHost(t,
		fakeRelease{tag: "v1.5.0"},
		fakeRelease{tag: "v1.6.0", prerelease: true},
	)
	e := newEnv(t, host)
	// Online, no channel resolves to the flagged release.
	if res, err := e.run(e.opts("latest", "v1.0.0")); err != nil || res.Tag != "v1.5.0" {
		t.Fatalf("online latest: %+v, %v", res, err)
	}
	fillCache(t, e, "v1.6.0") // an explicit exact install caches it
	if m, err := readMeta(e.updater.Cache.EntryDir("v1.6.0")); err != nil || !m.Prerelease {
		t.Fatalf("cache meta = %+v, %v", m, err)
	}
	goOffline(e)
	for _, sel := range []string{"latest", "1", "1.6"} {
		o := e.opts(sel, "v1.0.0")
		o.Offline = true
		res, err := e.run(o)
		switch sel {
		case "1.6":
			if !errors.Is(err, ErrNoCached) {
				t.Errorf("offline %s: %+v, %v; want ErrNoCached", sel, res, err)
			}
		default:
			if err != nil || res.Tag != "v1.5.0" {
				t.Errorf("offline %s: %+v, %v; want v1.5.0", sel, res, err)
			}
		}
		writeFile(t, e.exec, "installed binary", 0o755)
	}
	o := e.opts("v1.6.0", "v1.0.0")
	o.Offline = true
	if res, err := e.run(o); err != nil || res.Tag != "v1.6.0" {
		t.Fatalf("offline exact: %+v, %v", res, err)
	}
}
