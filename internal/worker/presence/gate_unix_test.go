//go:build !windows

package presence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var umasks = []int{0o022, 0o077}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// A reset or replacement through another name of the state directory must find
// the claim of the instance that published through the first one.
func TestClearProfileThroughAStateDirAlias(t *testing.T) {
	for _, um := range umasks {
		withUmask(t, um)
		for _, startVia := range []string{"alias", "physical"} {
			base := t.TempDir()
			phys := filepath.Join(base, "state")
			alias := filepath.Join(base, "alias")
			if err := os.Mkdir(phys, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(phys, alias); err != nil {
				t.Fatal(err)
			}
			pub := filepath.Join(t.TempDir(), "pub")
			if err := os.Mkdir(pub, 0o755); err != nil {
				t.Fatal(err)
			}
			first, second := alias, phys
			if startVia == "physical" {
				first, second = phys, alias
			}
			clk := newClock()
			p := newPub(t, rootLayout(t, pub, first), "inst-a", clk)
			start(t, p)
			if err := p.SetUsage([]byte(usageDoc(1))); err != nil {
				t.Fatal(err)
			}
			if !exists(filepath.Join(pub, "inst-a.usage.json")) {
				t.Fatal("no usage file")
			}
			if err := ClearProfile(rootLayout(t, pub, second)); err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"inst-a.json", "inst-a.usage.json"} {
				if exists(filepath.Join(pub, n)) {
					t.Fatalf("umask %o via %s: %s survived a reset through the other name", um, startVia, n)
				}
			}
			// And the other direction: a successor started through the other
			// name replaces the predecessor.
			p2 := newPub(t, rootLayout(t, pub, first), "inst-b", clk)
			start(t, p2)
			_ = p2.SetUsage([]byte(usageDoc(2)))
			p3 := newPub(t, rootLayout(t, pub, second), "inst-c", clk)
			start(t, p3)
			if exists(filepath.Join(pub, "inst-b.usage.json")) || exists(filepath.Join(pub, "inst-b.json")) {
				t.Fatal("the predecessor through the other name was kept")
			}
		}
	}
}

func TestProfileKeyFollowsTheDirectoryNotThePath(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	_ = os.Mkdir(a, 0o700)
	l := Layout{StateDir: a}
	k := l.profileKey()
	b := filepath.Join(base, "b")
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	if got := (Layout{StateDir: b}).profileKey(); got != k {
		t.Fatal("a moved state directory has another key")
	}
	// A different directory at the old path is another profile.
	_ = os.Mkdir(a, 0o700)
	if l.profileKey() == k {
		t.Fatal("a new directory at the old path shares the claim")
	}
}

func victim(t *testing.T) string {
	t.Helper()
	v := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(v, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	return v
}

func intact(t *testing.T, v string) {
	t.Helper()
	if b, err := os.ReadFile(v); err != nil || string(b) != "precious" {
		t.Fatalf("the victim file was changed: %q %v", b, err)
	}
}

func TestStagingDirectoryIsCleanedBeforeUse(t *testing.T) {
	for _, um := range umasks {
		withUmask(t, um)
		for _, mode := range []os.FileMode{0o700, 0o770, 0o777} {
			v := victim(t)
			pub := filepath.Join(t.TempDir(), "pub")
			_ = os.Mkdir(pub, 0o755)
			stage := filepath.Join(pub, ".stage")
			_ = os.Mkdir(stage, 0o700)
			l := rootLayout(t, pub, filepath.Join(t.TempDir(), "state"))
			_ = os.Mkdir(l.StateDir, 0o700)
			key := "profile-" + l.profileKey()
			// Links planted where the old code wrote its temporary file, a
			// random leftover, and a link in place of the claim.
			_ = os.Symlink(v, filepath.Join(stage, key+".new"))
			_ = os.Symlink(v, filepath.Join(stage, key))
			_ = os.Symlink(v, filepath.Join(stage, "tmp-1234"))
			_ = os.WriteFile(filepath.Join(stage, "junk"), []byte("x"), 0o600)
			_ = os.Mkdir(filepath.Join(stage, "dir"), 0o700)
			// A claim of another profile that is legitimate.
			other := "profile-0123456789abcdef"
			_ = os.WriteFile(filepath.Join(stage, other), []byte("other-inst"), 0o600)
			_ = os.Chmod(stage, mode)
			p := newPub(t, l, "inst-a", newClock())
			start(t, p)
			intact(t, v)
			if fi, err := os.Lstat(stage); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
				t.Fatalf("umask %o mode %o: staging directory %v %v", um, mode, fi, err)
			}
			for _, n := range []string{key + ".new", "tmp-1234", "junk", "dir"} {
				if exists(filepath.Join(stage, n)) {
					t.Errorf("umask %o mode %o: %s was kept", um, mode, n)
				}
			}
			if fi, err := os.Lstat(filepath.Join(stage, key)); err != nil || !fi.Mode().IsRegular() {
				t.Errorf("the claim is not a regular file: %v %v", fi, err)
			}
			if mode == 0o700 && !exists(filepath.Join(stage, other)) {
				t.Errorf("umask %o: the claim of another profile was removed", um)
			}
			ents, _ := os.ReadDir(pub)
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".stage-old") {
					t.Errorf("an aside staging directory was left: %s", e.Name())
				}
			}
			if got := (backend{l}).claimed(); got != "inst-a" {
				t.Fatalf("claim = %q", got)
			}
		}
	}
}

func TestClaimReadRefusesLinksAndWrongModes(t *testing.T) {
	pub := filepath.Join(t.TempDir(), "pub")
	_ = os.Mkdir(pub, 0o755)
	l := rootLayout(t, pub, filepath.Join(t.TempDir(), "state"))
	_ = os.Mkdir(l.StateDir, 0o700)
	p := newPub(t, l, "inst-a", newClock())
	start(t, p)
	b := backend{l}
	path, _ := b.claimPath()
	if b.claimed() != "inst-a" {
		t.Fatal("no claim")
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if got := b.claimed(); got != "" {
		t.Fatalf("a world-writable claim was trusted: %q", got)
	}
	_ = os.Remove(path)
	v := victim(t)
	_ = os.Symlink(v, path)
	if got := b.claimed(); got != "" {
		t.Fatalf("a claim link was followed: %q", got)
	}
}

func TestPlantedUsageFileIsRefusedByTheReader(t *testing.T) {
	for _, public := range []bool{false, true} {
		clk := newClock()
		var l Layout
		var r Reader
		var dir string
		var mode os.FileMode
		if public {
			pub := filepath.Join(t.TempDir(), "pub")
			_ = os.Mkdir(pub, 0o755)
			l = rootLayout(t, pub, filepath.Join(t.TempDir(), "s"))
			dir, mode, r = pub, 0o644, pubReader(l, clk)
		} else {
			l = userLayout(t)
			dir, mode, r = l.PrivateDir, 0o600, privReader(l, clk)
		}
		p := newPub(t, l, "u1", clk)
		start(t, p)
		path := filepath.Join(dir, "u1.usage.json")
		for name, doc := range map[string]string{
			"nested unknown": `{"schema":"minion.usage-meters/1","generated_at":1,"slots":[{"slot":{"scope":"user","kind":"session","label":"a","member":"x"},"meters":[]}]}`,
			"missing fields": `{"schema":"minion.usage-meters/1"}`,
			"trailing":       usageDoc(1) + "]",
		} {
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			_ = os.Chmod(path, mode)
			if _, err := r.ReadUsage(Candidate{Source: srcOf(public), Dir: dir, Instance: "u1"}); err == nil {
				t.Errorf("public=%v %s: planted document returned", public, name)
			}
			if err := p.SetUsage([]byte(doc)); err == nil {
				t.Errorf("public=%v %s: stored", public, name)
			}
		}
	}
}

func srcOf(public bool) Source {
	if public {
		return SourcePublic
	}
	return SourcePrivate
}

func TestHubLinkIsReadOnEveryPublication(t *testing.T) {
	l, clk := userLayout(t), newClock()
	p := newPub(t, l, "u1", clk)
	state := HubLinkConnected
	p.SetHubLink(func() string { return state })
	start(t, p)
	r := privReader(l, clk)
	if got := Best(r.Scan()).Record.HubLink; got != HubLinkConnected {
		t.Fatalf("link %q", got)
	}
	for _, s := range []string{HubLinkRegistrationPending, HubLinkDisconnected} {
		state = s
		clk.Add(time.Second)
		p.heartbeat()
		if got := Best(r.Scan()).Record.HubLink; got != s {
			t.Fatalf("want %q, got %q", s, got)
		}
	}
	state = "owner:alice" // never published
	p.heartbeat()
	if got := Best(r.Scan()).Record.HubLink; got != "" {
		t.Fatalf("invalid state published: %q", got)
	}
	state = HubLinkDisconnected
	p.heartbeat()
	if f := r.Discover(false); f[0].HubLink != HubLinkDisconnected {
		t.Fatalf("discover: %+v", f)
	}
	p.SetHubLink(nil)
	p.heartbeat()
	if got := Best(r.Scan()).Record.HubLink; got != "" {
		t.Fatalf("not cleared: %q", got)
	}
}

func TestSetUsageIfChecksJustBeforeTheRename(t *testing.T) {
	for _, public := range []bool{false, true} {
		clk := newClock()
		var l Layout
		var dir string
		if public {
			pub := filepath.Join(t.TempDir(), "pub")
			_ = os.Mkdir(pub, 0o755)
			l, dir = rootLayout(t, pub, filepath.Join(t.TempDir(), "s")), pub
		} else {
			l = userLayout(t)
			dir = l.PrivateDir
		}
		p := newPub(t, l, "u1", clk)
		start(t, p)
		path := filepath.Join(dir, "u1.usage.json")
		if err := p.SetUsageIf([]byte(usageDoc(1)), func() bool { return true }); err != nil {
			t.Fatal(err)
		}
		err := p.SetUsageIf([]byte(usageDoc(2)), func() bool { return false })
		if !errors.Is(err, ErrUsageSuperseded) {
			t.Fatalf("err = %v", err)
		}
		if got, _ := os.ReadFile(path); string(got) != usageDoc(1) || string(p.Usage()) != usageDoc(1) {
			t.Fatalf("a refused document replaced the old one: %q", got)
		}
		ents, _ := os.ReadDir(l.stageOrDir())
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "tmp-") {
				t.Errorf("staged file left: %s", e.Name())
			}
		}
	}
}

func TestPointersGoToEveryDiscoveryDirAndAreRemoved(t *testing.T) {
	for _, um := range umasks {
		withUmask(t, um)
		l := userLayout(t)
		base := t.TempDir()
		l.DiscoveryDir = filepath.Join(base, "svc", "presence-index")
		l.MoreDiscoveryDirs = []string{filepath.Join(base, "home", "presence-index")}
		clk := newClock()
		p := newPub(t, l, "svc1", clk)
		start(t, p)
		for _, d := range l.discoveryDirs() {
			r := Reader{UID: l.UID, DiscoveryDirs: []string{d}, Now: clk.Now}
			if res := Best(r.Scan()); res.Status != StatusRunning {
				t.Fatalf("umask %o: %s: %+v %v", um, d, res, res.Err)
			}
		}
		if err := ClearProfile(l); err != nil {
			t.Fatal(err)
		}
		for _, d := range l.discoveryDirs() {
			if ents, _ := os.ReadDir(d); len(ents) != 0 {
				t.Fatalf("pointer left in %s: %v", d, ents)
			}
		}
	}
}

func TestServiceStateDirs(t *testing.T) {
	home := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, 0o644)
	}
	write(".config/systemd/user/minion-worker.service", "[Service]\nEnvironment=\"MINION_STATE_DIR=/srv/my state\"\nExecStart=/x\n")
	write(".config/systemd/user/minion-worker.service.d/10.conf", "[Service]\nEnvironment=FOO=1 XDG_STATE_HOME=/var/svc-state\nEnvironment=XDG_STATE_HOME=%h/x\n")
	write(".config/minion/worker-login", "/usr/bin/minion\n/home/me/login-state\n")
	got := serviceStateDirs(home, os.Getuid())
	want := []string{"/srv/my state", "/var/svc-state/minion", "/home/me/login-state"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", got, want)
	}
	// A group-writable registration is not trusted.
	_ = os.Chmod(filepath.Join(home, ".config/minion/worker-login"), 0o666)
	if got := serviceStateDirs(home, os.Getuid()); len(got) != 2 {
		t.Fatalf("untrusted file read: %v", got)
	}
}

func TestReaderFindsAServiceWithItsOwnXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "login-xdg")) // the fresh login
	t.Setenv("MINION_STATE_DIR", "")
	svcState := filepath.Join(home, "svc-xdg", "minion")
	if err := os.MkdirAll(svcState, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, ".config/systemd/user")
	_ = os.MkdirAll(unit, 0o755)
	if err := os.WriteFile(filepath.Join(unit, "minion-worker.service"), []byte("[Service]\nEnvironment=XDG_STATE_HOME="+filepath.Join(home, "svc-xdg")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(unit, "minion-worker.service"), 0o644)
	// The service publishes without a pointer (an older release).
	l := Layout{Scope: ScopeUser, UID: me(), StateDir: svcState, PrivateDir: filepath.Join(svcState, "presence")}
	clk := newClock()
	start(t, newPub(t, l, "svc", clk))
	r := NewReader()
	r.PublicRoot = ""
	r.Now = clk.Now
	if res := Best(r.Scan()); res.Status != StatusRunning || res.Candidate.Instance != "svc" {
		t.Fatalf("%+v %v", res, res.Err)
	}
}

// Windows semantics through the seams: a reader that holds the file open blocks
// the unlink with a sharing violation.
func TestBlockedUnlinkIsRetriedThenKeptPending(t *testing.T) {
	errBusy := errors.New("sharing violation")
	oldRemove, oldRetry, oldSleep := removeFile, removeRetryable, removeSleep
	t.Cleanup(func() { removeFile, removeRetryable, removeSleep = oldRemove, oldRetry, oldSleep })
	removeRetryable = func(err error) bool { return errors.Is(err, errBusy) }
	var slept []time.Duration
	removeSleep = func(d time.Duration) { slept = append(slept, d) }

	l, clk := userLayout(t), newClock()
	p := newPub(t, l, "u1", clk)
	start(t, p)
	usage := filepath.Join(l.PrivateDir, "u1.usage.json")
	if err := p.SetUsage([]byte(usageDoc(1))); err != nil {
		t.Fatal(err)
	}

	// A short block is waited out.
	fails := 3
	removeFile = func(path string) error {
		if path == usage && fails > 0 {
			fails--
			return &os.PathError{Op: "remove", Path: path, Err: errBusy}
		}
		return oldRemove(path)
	}
	if err := p.ClearUsage(); err != nil || exists(usage) || len(slept) != 3 {
		t.Fatalf("short block: err=%v exists=%v slept=%v", err, exists(usage), slept)
	}
	if slept[2] <= slept[0] {
		t.Errorf("no backoff: %v", slept)
	}

	// A long block leaves the removal pending, with a marker.
	if err := p.SetUsage([]byte(usageDoc(2))); err != nil {
		t.Fatal(err)
	}
	slept = nil
	fails = 1000
	err := p.ClearUsage()
	if !errors.Is(err, errStillInUse) || !exists(usage) {
		t.Fatalf("long block: err=%v exists=%v", err, exists(usage))
	}
	if len(slept) != removeAttempts {
		t.Errorf("%d waits, want %d", len(slept), removeAttempts)
	}
	tomb := filepath.Join(l.PrivateDir, tombstonePrefix+"u1.usage.json"+tombstoneSuffix)
	if !exists(tomb) {
		t.Fatal("no pending marker")
	}
	// The marker is not an instance.
	if n := len(privReader(l, clk).Scan()); n != 1 {
		t.Fatalf("%d candidates", n)
	}
	// The viewer closes: the next heartbeat finishes the job.
	fails = 0
	clk.Add(time.Second)
	p.heartbeat()
	if exists(usage) || exists(tomb) {
		t.Fatalf("pending removal not finished: usage=%v marker=%v", exists(usage), exists(tomb))
	}
}

func TestPendingRemovalNeverDeletesANewerSnapshot(t *testing.T) {
	errBusy := errors.New("sharing violation")
	oldRemove, oldRetry, oldSleep := removeFile, removeRetryable, removeSleep
	t.Cleanup(func() { removeFile, removeRetryable, removeSleep = oldRemove, oldRetry, oldSleep })
	removeRetryable = func(err error) bool { return errors.Is(err, errBusy) }
	removeSleep = func(time.Duration) {}
	busy := true
	removeFile = func(path string) error {
		if busy && strings.HasSuffix(path, ".usage.json") {
			return &os.PathError{Op: "remove", Path: path, Err: errBusy}
		}
		return oldRemove(path)
	}
	l, clk := userLayout(t), newClock()
	p := newPub(t, l, "u1", clk)
	start(t, p)
	_ = p.SetUsage([]byte(usageDoc(1)))
	if err := p.ClearUsage(); err == nil {
		t.Fatal("expected a pending removal")
	}
	// A new snapshot arrives (a newer pairing), then the viewer lets go.
	if err := p.SetUsage([]byte(usageDoc(9))); err != nil {
		t.Fatal(err)
	}
	busy = false
	p.heartbeat()
	got, err := os.ReadFile(filepath.Join(l.PrivateDir, "u1.usage.json"))
	if err != nil || string(got) != usageDoc(9) {
		t.Fatalf("the newer snapshot was removed by a stale cleanup: %q %v", got, err)
	}
}

func TestPendingRemovalIsFinishedAtTheNextStart(t *testing.T) {
	errBusy := errors.New("sharing violation")
	oldRemove, oldRetry, oldSleep := removeFile, removeRetryable, removeSleep
	t.Cleanup(func() { removeFile, removeRetryable, removeSleep = oldRemove, oldRetry, oldSleep })
	removeRetryable = func(err error) bool { return errors.Is(err, errBusy) }
	removeSleep = func(time.Duration) {}
	removeFile = func(path string) error {
		if strings.HasSuffix(path, ".usage.json") {
			return &os.PathError{Op: "remove", Path: path, Err: errBusy}
		}
		return oldRemove(path)
	}
	l, clk := userLayout(t), newClock()
	a := newPub(t, l, "a", clk)
	start(t, a)
	_ = a.SetUsage([]byte(usageDoc(1)))
	if err := ClearProfile(l); err == nil {
		t.Fatal("expected the blocked removal to be reported")
	}
	if !exists(filepath.Join(l.PrivateDir, "a.usage.json")) {
		t.Fatal("setup")
	}
	removeFile = oldRemove // the reader is gone
	b := newPub(t, l, "b", clk)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Stop() })
	if exists(filepath.Join(l.PrivateDir, "a.usage.json")) {
		t.Fatal("the old usage document survived the next start")
	}
	_ = syscall.Getpid()
}
