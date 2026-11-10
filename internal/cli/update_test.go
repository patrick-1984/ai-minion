package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// fakeSource serves releases from memory.
type fakeSource struct {
	releases []version.Release
	assets   map[string][]byte // by URL
	listErr  error
	listed   int
}

func (f *fakeSource) Releases(context.Context) ([]version.Release, error) {
	f.listed++
	return f.releases, f.listErr
}

func (f *fakeSource) Open(_ context.Context, a version.Asset) (io.ReadCloser, error) {
	data, ok := f.assets[a.URL]
	if !ok {
		return nil, fmt.Errorf("no asset %s", a.URL)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// acceptAll is a verifier that accepts every signature: these tests cover the
// command wiring, the verification itself is tested in internal/version.
type acceptAll struct{}

func (acceptAll) VerifyBlob(_, _ []byte, _ string) error { return nil }

// releaseArchive packs a release archive in the format ArchiveName gives the
// test platform: a .zip on Windows, a .tar.gz elsewhere.
func releaseArchive(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	if runtime.GOOS != "windows" {
		return tarGz(t, name, data)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGz(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(data)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// addRelease publishes a release with a protocol range for the test platform.
func (f *fakeSource) addRelease(t *testing.T, tag, protoMin, protoMax string, prerelease bool) {
	t.Helper()
	archive := version.ArchiveName(tag, runtime.GOOS, runtime.GOARCH, map[bool]string{true: "7"}[runtime.GOARCH == "arm"])
	data := releaseArchive(t, version.BinaryName(runtime.GOOS), []byte("binary "+tag))
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	manifest := fmt.Sprintf(`{"schema":1,"version":%q,"protocol_range":{"min":%q,"max":%q},"artifacts":[{"name":%q,"sha256":%q}]}`,
		tag, protoMin, protoMax, archive, hexSum)
	files := map[string][]byte{
		archive:                               data,
		"checksums.txt":                       []byte(hexSum + "  " + archive + "\n"),
		"checksums.txt.sigstore.json":         []byte("{}"),
		"release-manifest.json":               []byte(manifest),
		"release-manifest.json.sigstore.json": []byte("{}"),
	}
	rel := version.Release{Tag: tag, Prerelease: prerelease}
	if f.assets == nil {
		f.assets = map[string][]byte{}
	}
	for name, content := range files {
		url := "mem://" + tag + "/" + name
		f.assets[url] = content
		rel.Assets = append(rel.Assets, version.Asset{Name: name, URL: url})
	}
	f.releases = append(f.releases, rel)
}

type harness struct {
	t      *testing.T
	src    *fakeSource
	exec   string
	env    map[string]string
	file   config.MapSource // the Worker config file
	gotAPI string
	gotDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, src: &fakeSource{}, exec: filepath.Join(dir, "minion"), env: map[string]string{}, file: config.MapSource{}}
	if err := os.WriteFile(h.exec, []byte("installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.env["MINION_UPDATE_CACHE_DIR"] = filepath.Join(dir, "cache")
	return h
}

func (h *harness) run(current string, args ...string) (stdout string, err error) {
	h.t.Helper()
	deps := updateDeps{
		newUpdater: func(apiURL, cacheDir string, out io.Writer) (*version.Updater, error) {
			h.gotAPI, h.gotDir = apiURL, cacheDir
			return &version.Updater{
				Source:   h.src,
				Verifier: func(context.Context, bool) (version.BlobVerifier, error) { return acceptAll{}, nil },
				Cache:    version.Cache{Dir: cacheDir},
				Out:      out,
			}, nil
		},
		lookupEnv:  func(k string) (string, bool) { v, ok := h.env[k]; return v, ok },
		current:    func() string { return current },
		execPath:   h.exec,
		loadConfig: func(string) (config.FileSource, error) { return h.file, nil },
	}
	cmd := newUpdateCmd(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func (h *harness) installed() string {
	h.t.Helper()
	b, err := os.ReadFile(h.exec)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func TestUpdateCommandRegistered(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"update"})
	if err != nil || cmd.Name() != "update" {
		t.Fatalf("update command not registered: %v", err)
	}
	for _, flag := range []string{"channel", "version", "allow-downgrade", "offline", "hub-proto-range", "update-api-url", "update-cache-dir"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("missing --%s", flag)
		}
	}
}

func TestUpdateCommandChannelFlag(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.4.1", "1.0", "1.1", false)
	h.src.addRelease(t, "v1.4.2", "1.0", "1.1", false)
	h.src.addRelease(t, "v1.5.0-rc.1", "1.0", "1.1", true)
	h.src.addRelease(t, "v2.0.0", "2.0", "2.0", false)

	out, err := h.run("v1.0.0", "--channel", "1.4")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.installed() != "binary v1.4.2" || !strings.Contains(out, "updated minion from v1.0.0 to v1.4.2") {
		t.Errorf("installed %q, output %q", h.installed(), out)
	}
	if h.gotAPI != "https://api.github.com" {
		t.Errorf("API URL = %q", h.gotAPI)
	}
}

func TestUpdateCommandDefaultsToLatestAndReadsEnv(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.4.2", "1.0", "1.1", false)
	h.src.addRelease(t, "v2.0.0", "2.0", "2.0", false)
	if _, err := h.run("v1.0.0"); err != nil || h.installed() != "binary v2.0.0" {
		t.Fatalf("default channel: %v, installed %q", err, h.installed())
	}
	if err := os.WriteFile(h.exec, []byte("installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.env["MINION_CHANNEL"] = "1"
	h.env["MINION_UPDATE_API_URL"] = "http://127.0.0.1:1"
	if _, err := h.run("v1.0.0"); err != nil || h.installed() != "binary v1.4.2" {
		t.Fatalf("env channel: %v, installed %q", err, h.installed())
	}
	if h.gotAPI != "http://127.0.0.1:1" {
		t.Errorf("API URL = %q", h.gotAPI)
	}
	// The flag wins over the environment.
	if err := os.WriteFile(h.exec, []byte("installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run("v1.0.0", "--channel", "v2.0.0"); err != nil || h.installed() != "binary v2.0.0" {
		t.Fatalf("flag over env: %v, installed %q", err, h.installed())
	}
}

func TestUpdateCommandUpToDate(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.4.2", "1.0", "1.1", false)
	out, err := h.run("v1.4.2", "--channel", "v1.4.2")
	if err != nil || !strings.Contains(out, "up to date (v1.4.2)") || h.installed() != "installed" {
		t.Fatalf("%v, %q, installed %q", err, out, h.installed())
	}
}

func TestUpdateCommandDowngradeNeedsFlag(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.4.1", "1.0", "1.1", false)
	_, err := h.run("v1.4.2", "--channel", "v1.4.1")
	if !errors.Is(err, version.ErrDowngrade) || h.installed() != "installed" {
		t.Fatalf("err = %v, installed %q", err, h.installed())
	}
	if _, err := h.run("v1.4.2", "--channel", "v1.4.1", "--allow-downgrade"); err != nil || h.installed() != "binary v1.4.1" {
		t.Fatalf("with --allow-downgrade: %v, installed %q", err, h.installed())
	}
}

func TestUpdateCommandHubProtocolFlag(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.6.0", "1.3", "1.4", false)
	_, err := h.run("v1.0.0", "--hub-proto-range", "1.0..1.1")
	if !errors.Is(err, version.ErrIncompatible) || !strings.Contains(err.Error(), "incompatible with the running Hub connection") {
		t.Fatalf("err = %v", err)
	}
	if h.installed() != "installed" {
		t.Error("binary changed")
	}
	if _, err := h.run("v1.0.0", "--hub-proto-range", "1.1..1.3"); err != nil || h.installed() != "binary v1.6.0" {
		t.Fatalf("overlapping range: %v, installed %q", err, h.installed())
	}
	if _, err := h.run("v1.0.0", "--hub-proto-range", "nonsense"); err == nil || !strings.Contains(err.Error(), "--hub-proto-range") {
		t.Fatalf("bad range: err = %v", err)
	}
}

func TestUpdateCommandHubRangeProvider(t *testing.T) {
	t.Cleanup(func() { SetHubRange(nil) })
	h := newHarness(t)
	h.src.addRelease(t, "v1.6.0", "1.3", "1.4", false)

	// Connected to a Hub whose range is disjoint: refused.
	SetHubRange(func(context.Context) (version.ProtoRange, bool, error) {
		return version.ProtoRange{Min: version.ProtoVersion{Major: 1, Minor: 0}, Max: version.ProtoVersion{Major: 1, Minor: 1}}, true, nil
	})
	if _, err := h.run("v1.0.0"); !errors.Is(err, version.ErrIncompatible) {
		t.Fatalf("connected: err = %v", err)
	}
	// Not connected: nothing to check.
	SetHubRange(func(context.Context) (version.ProtoRange, bool, error) { return version.ProtoRange{}, false, nil })
	if _, err := h.run("v1.0.0"); err != nil {
		t.Fatalf("not connected: %v", err)
	}
	// The flag overrides the provider.
	if err := os.WriteFile(h.exec, []byte("installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	SetHubRange(func(context.Context) (version.ProtoRange, bool, error) {
		return version.ProtoRange{}, false, errors.New("boom")
	})
	if _, err := h.run("v1.0.0"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("provider error: %v", err)
	}
	if _, err := h.run("v1.0.0", "--hub-proto-range", "1.3..1.4"); err != nil {
		t.Fatalf("flag overrides provider: %v", err)
	}
}

func TestUpdateCommandRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"--channel", "stable"}, {"--channel", "1.x"}, {"extra"}} {
		if _, err := h.run("v1.0.0", args...); err == nil {
			t.Errorf("args %v accepted", args)
		}
	}
	if h.src.listed != 0 {
		t.Error("the release host was contacted for invalid input")
	}
}

func TestUpdateCommandOfflineSkipsTheNetwork(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.4.2", "1.0", "1.1", false)
	if _, err := h.run("v1.0.0", "--channel", "1.4"); err != nil {
		t.Fatal(err)
	}
	listed := h.src.listed
	if err := os.WriteFile(h.exec, []byte("installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.src.listErr = errors.New("no network")
	out, err := h.run("v1.0.0", "--channel", "1.4", "--offline")
	if err != nil || h.installed() != "binary v1.4.2" || h.src.listed != listed {
		t.Fatalf("offline: %v, installed %q, listed %d->%d\n%s", err, h.installed(), listed, h.src.listed, out)
	}
	if _, err := h.run("v1.0.0", "--channel", "3", "--offline"); !errors.Is(err, version.ErrNoCached) {
		t.Fatalf("offline without a match: %v", err)
	}
}

func TestUpdateCommandNetworkErrorMentionsOffline(t *testing.T) {
	h := newHarness(t)
	h.src.listErr = fmt.Errorf("%w: dial tcp: no route", version.ErrNetwork)
	_, err := h.run("v1.0.0")
	if !errors.Is(err, version.ErrNetwork) || !strings.Contains(err.Error(), "--offline") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultUpdaterValidatesAPIURL(t *testing.T) {
	deps := defaultUpdateDeps()
	if _, err := deps.newUpdater("http://example.com", t.TempDir(), io.Discard); err == nil {
		t.Error("a plain-http remote API URL was accepted")
	}
	u, err := deps.newUpdater("https://api.github.com", t.TempDir(), io.Discard)
	if err != nil || u.Source == nil || u.Verifier == nil {
		t.Fatalf("default updater: %+v, %v", u, err)
	}
}

func TestUpdateCommandVersionFlagInstallsAnExactRC(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v0.1.0-rc.1", "1.0", "1.1", true)
	h.src.addRelease(t, "v0.1.0-rc.2", "1.0", "1.1", true)
	h.src.addRelease(t, "v0.0.9", "1.0", "1.1", false)

	// The L16 / Story 9.7 invocation.
	out, err := h.run("v0.0.9", "--version", "v0.1.0-rc.2")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.installed() != "binary v0.1.0-rc.2" || !strings.Contains(out, "updated minion from v0.0.9 to v0.1.0-rc.2") {
		t.Errorf("installed %q, output %q", h.installed(), out)
	}
	// The flag wins over a channel from the environment and the config file.
	h.env["MINION_CHANNEL"] = "0.0"
	h.file["channel"] = "0.0"
	if err := os.WriteFile(h.exec, []byte("installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run("v0.0.9", "--version", "v0.1.0-rc.1"); err != nil || h.installed() != "binary v0.1.0-rc.1" {
		t.Fatalf("--version over the saved channel: %v, installed %q", err, h.installed())
	}
	// An exact stable version works too.
	if _, err := h.run("v0.1.0-rc.1", "--version", "v0.0.9", "--allow-downgrade"); err != nil || h.installed() != "binary v0.0.9" {
		t.Fatalf("exact stable: %v, installed %q", err, h.installed())
	}
}

func TestUpdateCommandVersionFlagRefusesChannelsAndMixing(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.4.2", "1.0", "1.1", false)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"minor channel":     {[]string{"--version", "1.4"}, "--channel"},
		"major channel":     {[]string{"--version", "1"}, "--channel"},
		"latest":            {[]string{"--version", "latest"}, "--channel"},
		"not a version":     {[]string{"--version", "stable"}, "vX.Y.Z"},
		"empty":             {[]string{"--version", ""}, "vX.Y.Z"},
		"both flags":        {[]string{"--version", "v1.4.2", "--channel", "1.4"}, "cannot be combined"},
		"both, same exact":  {[]string{"--channel", "v1.4.2", "--version", "v1.4.2"}, "cannot be combined"},
		"both with offline": {[]string{"--version", "v1.4.2", "--channel", "latest", "--offline"}, "cannot be combined"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.run("v1.0.0", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
			if h.src.listed != 0 {
				t.Error("the release host was contacted")
			}
			if h.installed() != "installed" {
				t.Error("binary changed")
			}
		})
	}
	if h.gotAPI != "" {
		t.Error("an updater was built for a refused input")
	}
}

// TestUpdateCommandUsesTheSavedSettings: the Worker config file takes part in
// the precedence chain, below the environment and the flags.
func TestUpdateCommandUsesTheSavedSettings(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.4.1", "1.0", "1.1", false)
	h.src.addRelease(t, "v1.4.2", "1.0", "1.1", false)
	h.src.addRelease(t, "v2.0.0", "2.0", "2.0", false)
	reset := func() {
		if err := os.WriteFile(h.exec, []byte("installed"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A saved channel pin must hold: no jump to the newest major.
	h.file["channel"] = "1.4"
	if _, err := h.run("v1.0.0"); err != nil || h.installed() != "binary v1.4.2" {
		t.Fatalf("saved channel: %v, installed %q", err, h.installed())
	}
	// The environment wins over the file.
	reset()
	h.env["MINION_CHANNEL"] = "2"
	if _, err := h.run("v1.0.0"); err != nil || h.installed() != "binary v2.0.0" {
		t.Fatalf("env over file: %v, installed %q", err, h.installed())
	}
	// The flag wins over the environment.
	reset()
	if _, err := h.run("v1.0.0", "--channel", "v1.4.1"); err != nil || h.installed() != "binary v1.4.1" {
		t.Fatalf("flag over env: %v, installed %q", err, h.installed())
	}
	// Saved API URL and cache directory are used when nothing overrides them.
	delete(h.env, "MINION_UPDATE_CACHE_DIR")
	h.file["update_api_url"] = "http://127.0.0.1:9"
	h.file["update_cache_dir"] = filepath.Join(t.TempDir(), "saved-cache")
	reset()
	if _, err := h.run("v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if h.gotAPI != "http://127.0.0.1:9" || h.gotDir != h.file["update_cache_dir"] {
		t.Errorf("API %q, cache %q; want the saved values", h.gotAPI, h.gotDir)
	}
}

func TestLoadWorkerConfigReadsTheStateDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, workerConfigName), []byte(`{"channel":"1.4"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := loadWorkerConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := f.Lookup("channel"); !ok || v != "1.4" {
		t.Errorf("channel = %q, %v", v, ok)
	}
	// A state directory without a file is an empty file, not an error.
	if f, err := loadWorkerConfig(t.TempDir()); err != nil {
		t.Fatal(err)
	} else if _, ok, _ := f.Lookup("channel"); ok {
		t.Error("an empty state directory yielded a value")
	}
	// A broken file is an error, never silently ignored.
	if err := os.WriteFile(filepath.Join(dir, workerConfigName), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkerConfig(dir); err == nil {
		t.Error("a corrupt config file was accepted")
	}
}
