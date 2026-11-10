//go:build !windows

package version

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapWriter simulates a writer that replaces the cache entry or the staged
// executable at the moment the updater announces the install, that is after
// every verification, with an unsigned executable.
type swapWriter struct {
	t       *testing.T
	prefix  string
	swap    func()
	swapped bool
}

func (w *swapWriter) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), w.prefix) && !w.swapped {
		w.swap()
		w.swapped = true
	}
	return len(p), nil
}

const unsignedMarker = "UNSIGNED-CACHE-SWAP"

// TestUpdateRefusesUnsafeCacheBeforeUsingIt: a cache that a lower-trust user
// could write or rename is refused before anything is downloaded, written or
// installed. These are the preconditions of the final audit's cache-swap proof,
// whose original test (a 0777 cache parent) now ends in a refusal.
func TestUpdateRefusesUnsafeCacheBeforeUsingIt(t *testing.T) {
	cases := map[string]func(t *testing.T, e *env){
		"cache is world-writable": func(t *testing.T, e *env) {
			mustMkdir(t, e.cache, 0o777)
		},
		"cache is group-writable": func(t *testing.T, e *env) {
			mustMkdir(t, e.cache, 0o770)
		},
		"private cache below a world-writable parent": func(t *testing.T, e *env) {
			parent := filepath.Join(filepath.Dir(e.cache), "shared")
			mustMkdir(t, parent, 0o777)
			e.cache = filepath.Join(parent, "cache")
			e.updater.Cache = Cache{Dir: e.cache}
		},
		"cache reached through a link in a world-writable directory": func(t *testing.T, e *env) {
			real := filepath.Join(filepath.Dir(e.cache), "real-cache")
			mustMkdir(t, real, 0o700)
			shared := filepath.Join(filepath.Dir(e.cache), "shared")
			mustMkdir(t, shared, 0o777)
			e.cache = filepath.Join(shared, "link")
			if err := os.Symlink(real, e.cache); err != nil {
				t.Fatal(err)
			}
			e.updater.Cache = Cache{Dir: e.cache}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, standardHost(t))
			setup(t, e)
			sw := &swapWriter{t: t, prefix: "installing v1.2.3", swap: func() {
				entry := filepath.Join(e.cache, "v1.2.3")
				_ = os.Rename(entry, entry+".moved")
				_ = os.Mkdir(entry, 0o755)
				_ = os.WriteFile(filepath.Join(entry, "minion"), []byte(unsignedMarker), 0o755)
			}}
			e.updater.Out = sw
			_, err := e.run(e.opts("v1.2.3", "v1.0.0"))
			if !errors.Is(err, ErrUntrustedCache) {
				t.Fatalf("err = %v, want ErrUntrustedCache", err)
			}
			if sw.swapped {
				t.Error("the updater went on to install from an unsafe cache")
			}
			if e.installed() != "installed binary" {
				t.Errorf("installed %q", e.installed())
			}
			if ents, _ := os.ReadDir(e.cache); len(ents) != 0 {
				t.Errorf("an unsafe cache was written to: %v", ents)
			}
		})
	}
}

// A root-owned sticky directory such as /tmp above a private cache is how a
// legitimate cache under a shared temporary directory looks, and stays usable.
func TestUpdateAcceptsPrivateCacheBelowStickyDirectory(t *testing.T) {
	e := newEnv(t, standardHost(t))
	if err := os.Chmod(filepath.Dir(e.cache), 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(e.opts("v1.2.3", "v1.0.0")); err != nil {
		t.Fatalf("a private cache below a sticky directory was refused: %v", err)
	}
	if e.installed() != "minion v1.2.3" {
		t.Errorf("installed %q", e.installed())
	}
}

func TestUpdateOfflineRefusesUnsafeCache(t *testing.T) {
	cases := map[string]func(t *testing.T, e *env){
		"cache became world-writable": func(t *testing.T, e *env) {
			if err := os.Chmod(e.cache, 0o777); err != nil {
				t.Fatal(err)
			}
		},
		"parent became writable by others": func(t *testing.T, e *env) {
			if err := os.Chmod(filepath.Dir(e.cache), 0o777); err != nil {
				t.Fatal(err)
			}
		},
		"cache replaced by a link in a world-writable directory": func(t *testing.T, e *env) {
			dir := filepath.Dir(e.cache)
			shared := filepath.Join(dir, "shared")
			mustMkdir(t, shared, 0o777)
			moved := filepath.Join(dir, "moved-cache")
			if err := os.Rename(e.cache, moved); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(shared, "link")
			if err := os.Symlink(moved, link); err != nil {
				t.Fatal(err)
			}
			e.cache = link
			e.updater.Cache = Cache{Dir: link}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, standardHost(t))
			fillCache(t, e, "v1.2.3")
			goOffline(e)
			setup(t, e)
			o := e.opts("v1.2.3", "v1.0.0")
			o.Offline = true
			if _, err := e.run(o); !errors.Is(err, ErrUntrustedCache) {
				t.Fatalf("err = %v, want ErrUntrustedCache", err)
			}
			if e.installed() != "installed binary" {
				t.Errorf("installed %q from an unsafe cache", e.installed())
			}
		})
	}
}

// A cached file that is a link is not followed: the entry is unusable.
func TestUpdateOfflineRefusesLinkedCacheFiles(t *testing.T) {
	for _, name := range []string{"archive", "checksums.txt", "meta.json", "minion"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, standardHost(t))
			fillCache(t, e, "v1.2.3")
			goOffline(e)
			dir := e.updater.Cache.EntryDir("v1.2.3")
			p := filepath.Join(dir, name)
			if name == "archive" {
				p = archivePath(t, dir)
			}
			copyTo := filepath.Join(filepath.Dir(e.cache), "copy-of-"+filepath.Base(p))
			if err := os.Rename(p, copyTo); err != nil { // same bytes, but outside the entry
				t.Fatal(err)
			}
			if err := os.Symlink(copyTo, p); err != nil {
				t.Fatal(err)
			}
			o := e.opts("v1.2.3", "v1.0.0")
			o.Offline = true
			_, err := e.run(o)
			if !errors.Is(err, ErrNoCached) || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("err = %v", err)
			}
			if e.installed() != "installed binary" {
				t.Errorf("installed %q", e.installed())
			}
		})
	}
}

// A user that shares the cache owner's rights is trusted by design. This test
// shows the second line of defence anyway: even when the cache entry is
// replaced after verification, the bytes that were not extracted from the signed
// archive are never installed.
func TestUpdateOnlineInstallsNothingSwappedAfterVerification(t *testing.T) {
	e := newEnv(t, standardHost(t))
	sw := &swapWriter{t: t, prefix: "installing v1.2.3", swap: func() {
		entry := filepath.Join(e.cache, "v1.2.3")
		if err := os.Rename(entry, entry+".verified-moved"); err != nil {
			t.Fatal(err)
		}
		mustMkdir(t, entry, 0o700)
		writeFile(t, filepath.Join(entry, "minion"), unsignedMarker, 0o700)
	}}
	e.updater.Out = sw
	_, err := e.run(e.opts("v1.2.3", "v1.0.0"))
	if !sw.swapped {
		t.Fatal("the swap did not run")
	}
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if e.installed() != "installed binary" {
		t.Errorf("installed %q", e.installed())
	}
	assertNoTempFiles(t, filepath.Dir(e.exec))
}

func TestUpdateOfflineInstallsNothingSwappedAfterVerification(t *testing.T) {
	e := newEnv(t, standardHost(t))
	fillCache(t, e, "v1.2.3")
	goOffline(e)
	sw := &swapWriter{t: t, prefix: "installing cached v1.2.3", swap: func() {
		stages, _ := filepath.Glob(filepath.Join(e.cache, ".stage-*", "minion"))
		if len(stages) != 1 {
			t.Fatalf("staged executables: %v", stages)
		}
		writeFile(t, stages[0], unsignedMarker, 0o700)
	}}
	e.updater.Out = sw
	o := e.opts("v1.2.3", "v1.0.0")
	o.Offline = true
	_, err := e.run(o)
	if !sw.swapped {
		t.Fatal("the swap did not run")
	}
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if e.installed() != "installed binary" {
		t.Errorf("installed %q", e.installed())
	}
	assertNoTempFiles(t, filepath.Dir(e.exec))
}

// An unmodified cache entry still installs, online and offline, through a
// private cache: the new checks do not turn a normal update into a refusal.
func TestUpdateSafeCacheStillInstallsOnlineAndOffline(t *testing.T) {
	e := newEnv(t, standardHost(t))
	fillCache(t, e, "v1.2.3")
	goOffline(e)
	o := e.opts("v1.2.3", "v1.0.0")
	o.Offline = true
	res, err := e.run(o)
	if err != nil || !res.FromCache || e.installed() != "minion v1.2.3" {
		t.Fatalf("offline = %+v, %v, installed %q", res, err, e.installed())
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".minion-update-") {
			t.Errorf("temporary file %s left next to the executable", e.Name())
		}
	}
}

func mustMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
