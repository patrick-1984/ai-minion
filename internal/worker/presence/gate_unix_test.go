//go:build !windows

package presence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// A failed unlink must not lose the way to the public files: the claim stays
// until both are gone, so a second clear finishes the job.
func TestClearProfileKeepsTheClaimUntilBothFilesAreGone(t *testing.T) {
	pub := filepath.Join(t.TempDir(), "pub")
	if err := os.Mkdir(pub, 0o755); err != nil {
		t.Fatal(err)
	}
	l := rootLayout(t, pub, filepath.Join(t.TempDir(), "state"))
	if err := os.MkdirAll(l.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := newPub(t, l, "inst-a", newClock())
	start(t, p)
	if err := p.SetUsage([]byte(usageDoc(1))); err != nil {
		t.Fatal(err)
	}
	usage := filepath.Join(pub, "inst-a.usage.json")
	oldRemove := removeFile
	t.Cleanup(func() { removeFile = oldRemove })
	removeFile = func(path string) error {
		if path == usage {
			return errors.New("transient unlink failure")
		}
		return oldRemove(path)
	}
	if err := ClearProfile(l); err == nil {
		t.Fatal("the failed unlink was not reported")
	}
	if !exists(usage) {
		t.Fatal("setup: the usage file should have survived")
	}
	removeFile = oldRemove // the failure clears
	if err := ClearProfile(l); err != nil {
		t.Fatal(err)
	}
	if exists(usage) || exists(filepath.Join(pub, "inst-a.json")) {
		t.Fatal("the second clear left the public files readable")
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

// Several root profiles starting against an empty public directory must all
// publish: the staging directory is created once and the others accept it, and
// nobody removes a temporary file another Worker has in flight.
func TestConcurrentRootStartsAgainstAnEmptyPublicDirectory(t *testing.T) {
	for round := 0; round < 20; round++ {
		pub := filepath.Join(t.TempDir(), "pub")
		if err := os.Mkdir(pub, 0o755); err != nil {
			t.Fatal(err)
		}
		const n = 6
		pubs := make([]*Publisher, n)
		for i := range pubs {
			state := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatal(err)
			}
			pubs[i] = newPub(t, rootLayout(t, pub, state), "inst-"+strconv.Itoa(i), newClock())
		}
		errs := make(chan error, n)
		var wg sync.WaitGroup
		for _, p := range pubs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- p.Start(context.Background())
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: a concurrent start failed: %v", round, err)
			}
		}
		for _, p := range pubs {
			_ = p.Stop()
		}
	}
}

func TestStagingKeepsATemporaryFileInFlightInAnotherWorker(t *testing.T) {
	pub := filepath.Join(t.TempDir(), "pub")
	_ = os.Mkdir(pub, 0o755)
	stage := filepath.Join(pub, ".stage")
	_ = os.Mkdir(stage, 0o700)
	tmp := filepath.Join(stage, "tmp-abcd")
	// atomicWrite has chmodded the file to its final mode, not yet renamed it.
	if err := os.WriteFile(tmp, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		t.Fatal(err)
	}
	l := rootLayout(t, pub, filepath.Join(t.TempDir(), "state"))
	_ = os.Mkdir(l.StateDir, 0o700)
	start(t, newPub(t, l, "inst-a", newClock()))
	if !exists(tmp) {
		t.Fatal("a temporary file in flight was removed by another Worker's start")
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

// The old marker may survive the replacement (a scanner holds it without delete
// sharing, so it cannot be dropped): it must still not remove the new file.
func TestPendingRemovalMarkerThatSurvivesNeverDeletesANewerSnapshot(t *testing.T) {
	errBusy := errors.New("sharing violation")
	oldRemove, oldRetry, oldSleep := removeFile, removeRetryable, removeSleep
	t.Cleanup(func() { removeFile, removeRetryable, removeSleep = oldRemove, oldRetry, oldSleep })
	removeRetryable = func(err error) bool { return errors.Is(err, errBusy) }
	removeSleep = func(time.Duration) {}
	busy := true
	removeFile = func(path string) error {
		if busy && (strings.HasSuffix(path, ".usage.json") || strings.HasSuffix(path, tombstoneSuffix)) {
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
	clk.Add(time.Second)
	if err := p.SetUsage([]byte(usageDoc(99))); err != nil {
		t.Fatal(err)
	}
	tomb := filepath.Join(l.PrivateDir, tombstonePrefix+"u1.usage.json"+tombstoneSuffix)
	if !exists(tomb) {
		t.Fatal("setup: the marker should have survived")
	}
	busy = false
	p.heartbeat()
	got, err := os.ReadFile(filepath.Join(l.PrivateDir, "u1.usage.json"))
	if err != nil || string(got) != usageDoc(99) {
		t.Fatalf("the newer snapshot was removed by a stale marker: %q %v", got, err)
	}
	if exists(tomb) {
		t.Fatal("the stale marker was kept")
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

// The Hub link is wired through the options, so the very first record carries
// it (SetHubLink after Start would leave the field absent until a heartbeat).
func TestFirstRecordCarriesTheHubLink(t *testing.T) {
	l, clk := userLayout(t), newClock()
	p := newPub(t, l, "u1", clk, func(o *Options) { o.HubLink = func() string { return HubLinkRegistrationPending } })
	start(t, p)
	if got := Best(privReader(l, clk).Scan()).Record.HubLink; got != HubLinkRegistrationPending {
		t.Fatalf("hub_link of the first record = %q", got)
	}
}
