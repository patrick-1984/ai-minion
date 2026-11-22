//go:build !windows

package presence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.UnixMilli(1_795_000_000_000)} }
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func me() int { return os.Geteuid() }

func userLayout(t *testing.T) Layout {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return Layout{Scope: ScopeUser, UID: me(), StateDir: state, PrivateDir: filepath.Join(state, "presence")}
}

// rootLayout injects a temporary publication root and treats the test's own
// uid as "root".
func rootLayout(t *testing.T, pub, state string) Layout {
	t.Helper()
	return Layout{Scope: ScopeRoot, UID: me(), RootUID: me(), StateDir: state, PublicRoot: pub}
}

func newPub(t *testing.T, l Layout, id string, clk *clock, mod ...func(*Options)) *Publisher {
	t.Helper()
	o := Options{Layout: l, Instance: id, Version: "v0.19.0", PID: 4242, Now: clk.Now,
		Interval: time.Hour, HubOrigin: func() string { return "https://hub.example:8443/secret/path?token=x" }}
	for _, m := range mod {
		m(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func start(t *testing.T, p *Publisher) {
	t.Helper()
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func privReader(l Layout, clk *clock) Reader {
	return Reader{UID: l.UID, PrivateDirs: []string{l.PrivateDir}, Now: clk.Now}
}

func pubReader(l Layout, clk *clock) Reader {
	return Reader{UID: l.UID, RootUID: l.RootUID, PublicRoot: l.PublicRoot, Now: clk.Now}
}

func withUmask(t *testing.T, m int) {
	old := syscall.Umask(m)
	t.Cleanup(func() { syscall.Umask(old) })
}

func TestUserLifecycleRunningStoppedCrash(t *testing.T) {
	for _, um := range []int{0o022, 0o077} {
		withUmask(t, um)
		l, clk := userLayout(t), newClock()
		p := newPub(t, l, "inst-a", clk)
		start(t, p)
		if m := mode(t, l.PrivateDir); m != 0o700 {
			t.Fatalf("umask %o: dir mode %o", um, m)
		}
		rec := filepath.Join(l.PrivateDir, "inst-a.json")
		if m := mode(t, rec); m != 0o600 {
			t.Fatalf("umask %o: record mode %o", um, m)
		}
		r := privReader(l, clk)
		res := Best(r.Scan())
		if res.Status != StatusRunning || res.Record.HubOrigin != "https://hub.example:8443" || res.Record.OSScope != ScopeUser || res.Record.PID != 4242 {
			t.Fatalf("running: %+v err=%v", res, res.Err)
		}
		// Crash: no stop marker; the record turns stale on its own.
		clk.Add(89 * time.Second)
		if got := Best(r.Scan()).Status; got != StatusRunning {
			t.Fatalf("89 s: %s", got)
		}
		clk.Add(2 * time.Second)
		if got := Best(r.Scan()).Status; got != StatusStale {
			t.Fatalf("91 s: %s", got)
		}
		// Clean stop.
		clk.Add(time.Second)
		if err := p.Stop(); err != nil {
			t.Fatal(err)
		}
		if res := Best(r.Scan()); res.Status != StatusStopped {
			t.Fatalf("stopped: %+v", res)
		}
	}
}

func TestHeartbeatEveryInterval(t *testing.T) {
	l, clk := userLayout(t), newClock()
	p := newPub(t, l, "hb", clk, func(o *Options) { o.Interval = 10 * time.Millisecond })
	start(t, p)
	r := privReader(l, clk)
	first := Best(r.Scan()).Record.UpdatedAt
	clk.Add(30 * time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if Best(r.Scan()).Record.UpdatedAt > first {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no heartbeat written")
}

func TestTwoUsersAreIndependent(t *testing.T) {
	clk := newClock()
	la, lb := userLayout(t), userLayout(t)
	start(t, newPub(t, la, "alice-1", clk))
	start(t, newPub(t, lb, "bob-1", clk))
	if got := Best(privReader(la, clk).Scan()); got.Record.Instance != "alice-1" {
		t.Fatalf("%+v", got)
	}
	if got := Best(privReader(lb, clk).Scan()); got.Record.Instance != "bob-1" {
		t.Fatalf("%+v", got)
	}
	// Another uid must not trust the directory: it is not theirs.
	other := Reader{UID: me() + 1, PrivateDirs: []string{la.PrivateDir}, Now: clk.Now}
	res := other.Scan()
	if len(res) != 1 || res[0].Status != StatusUnavailable {
		t.Fatalf("foreign reader: %+v", res)
	}
	// A record whose owner differs from the expected uid is refused too.
	rd := Reader{UID: me() + 1, Now: clk.Now}
	if _, err := readPrivateFile(la.PrivateDir, "alice-1.json", rd.UID, MaxRecordSize); err == nil {
		t.Fatal("wrong-owner record accepted")
	}
}

func TestRelocatedProfileIsDiscoverable(t *testing.T) {
	clk := newClock()
	l := userLayout(t)
	l.DiscoveryDir = filepath.Join(t.TempDir(), "defstate", "presence-index")
	p := newPub(t, l, "reloc", clk)
	start(t, p)
	if m := mode(t, l.DiscoveryDir); m != 0o700 {
		t.Fatalf("discovery dir %o", m)
	}
	ents, _ := os.ReadDir(l.DiscoveryDir)
	if len(ents) != 1 || mode(t, filepath.Join(l.DiscoveryDir, ents[0].Name())) != 0o600 {
		t.Fatalf("discovery entries: %v", ents)
	}
	r := Reader{UID: me(), DiscoveryDirs: []string{l.DiscoveryDir}, Now: clk.Now}
	if got := Best(r.Scan()); got.Status != StatusRunning || got.Record.Instance != "reloc" {
		t.Fatalf("%+v", got)
	}
	// A pointer to a hostile directory is not followed blindly.
	if err := ClearProfile(l); err != nil {
		t.Fatal(err)
	}
	if res := r.Scan(); len(res) != 0 {
		t.Fatalf("after clear: %+v", res)
	}
}

func TestRootProfilesGetOneRecordEach(t *testing.T) {
	for _, um := range []int{0o022, 0o077} {
		withUmask(t, um)
		clk := newClock()
		pub := filepath.Join(t.TempDir(), "run", "minion-presence")
		a := rootLayout(t, pub, "/state/a")
		b := rootLayout(t, pub, "/state/b")
		pa, pb := newPub(t, a, "root-a", clk), newPub(t, b, "root-b", clk)
		start(t, pa)
		start(t, pb)
		if m := mode(t, pub); m != 0o755 {
			t.Fatalf("umask %o: public dir %o", um, m)
		}
		if m := mode(t, filepath.Join(pub, ".stage")); m != 0o700 {
			t.Fatalf("stage dir %o", m)
		}
		for _, n := range []string{"root-a.json", "root-b.json"} {
			if m := mode(t, filepath.Join(pub, n)); m != 0o644 {
				t.Fatalf("%s mode %o", n, m)
			}
		}
		if ents, _ := os.ReadDir(filepath.Join(pub, ".stage")); len(ents) != 1 { // two claims, no temp
			names := []string{}
			for _, e := range ents {
				names = append(names, e.Name())
			}
			if len(ents) != 2 {
				t.Fatalf("stage leaves %v", names)
			}
			for _, e := range ents {
				if len(e.Name()) < 8 || e.Name()[:8] != "profile-" {
					t.Fatalf("temp left in stage: %v", names)
				}
			}
		}
		res := pubReader(a, clk).Scan()
		if len(res) != 2 || res[0].Status != StatusRunning || res[1].Status != StatusRunning || res[0].Record.OSScope != ScopeRoot {
			t.Fatalf("%+v", res)
		}
		// Stopping one leaves the other running; its own file keeps the marker.
		if err := pa.Stop(); err != nil {
			t.Fatal(err)
		}
		got := map[string]Status{}
		for _, r := range pubReader(a, clk).Scan() {
			got[r.Candidate.Instance] = r.Status
		}
		if got["root-a"] != StatusStopped || got["root-b"] != StatusRunning {
			t.Fatalf("%v", got)
		}
	}
}

func TestRootNeverUsesTheSocketDirectory(t *testing.T) {
	l := rootLayout(t, filepath.Join(t.TempDir(), "minion"), "/s")
	if _, err := New(Options{Layout: l, Instance: "x", Version: "v"}); err == nil {
		t.Fatal("the socket directory name was accepted")
	}
	if PublicRootFor("linux") != "/run/minion-presence" || PublicRootFor("darwin") != "/var/run/minion-presence" || PublicRootFor("windows") != "" {
		t.Fatal("public roots")
	}
}

func TestDefaultLayouts(t *testing.T) {
	env := func(state string) (e pathEnvT) { return pathEnv(state) }
	l, err := layoutFor("/data/state", 0, "linux", env("/home/u/.local/state").e())
	if err != nil || l.Scope != ScopeRoot || l.PublicRoot != "/run/minion-presence" {
		t.Fatalf("%+v %v", l, err)
	}
	l, _ = layoutFor("/var/root-state", 0, "darwin", env("/x").e())
	if l.PublicRoot != "/var/run/minion-presence" {
		t.Fatalf("%+v", l)
	}
	l, _ = layoutFor("/srv/alice-state", 1001, "linux", env("/home/alice/.local/state").e())
	if l.Scope != ScopeUser || l.PrivateDir != "/srv/alice-state/presence" || l.DiscoveryDir != "/home/alice/.local/state/minion/presence-index" {
		t.Fatalf("relocated: %+v", l)
	}
	// A service whose own XDG default differs from the home fallback of a
	// fresh login gets a pointer in both places.
	if got := l.discoveryDirs(); len(got) != 2 || got[1] != "/home/x/.local/state/minion/presence-index" {
		t.Fatalf("discovery dirs %v", got)
	}
	l, _ = layoutFor("/home/alice/.local/state/minion", 1001, "linux", env("/home/alice/.local/state").e())
	// The profile sits at its own XDG default; a login without XDG_STATE_HOME
	// looks in the home fallback, so the pointer goes there.
	if l.DiscoveryDir != "/home/x/.local/state/minion/presence-index" || l.PrivateDir != "/home/alice/.local/state/minion/presence" {
		t.Fatalf("default: %+v", l)
	}
	l, _ = layoutFor(`C:\Users\a\state`, -1, "windows", env("/x").e())
	if l.Scope != ScopeUser {
		t.Fatalf("windows: %+v", l)
	}
}

func TestHostilePaths(t *testing.T) {
	clk := newClock()
	t.Run("presence dir is a symlink", func(t *testing.T) {
		l := userLayout(t)
		target := filepath.Join(t.TempDir(), "elsewhere")
		_ = os.Mkdir(target, 0o700)
		if err := os.Symlink(target, l.PrivateDir); err != nil {
			t.Fatal(err)
		}
		if err := newPub(t, l, "x", clk).Start(context.Background()); err == nil {
			t.Fatal("started through a symlinked directory")
		}
		if ents, _ := os.ReadDir(target); len(ents) != 0 {
			t.Fatalf("wrote through the link: %v", ents)
		}
	})
	t.Run("world-writable parent", func(t *testing.T) {
		l := userLayout(t)
		_ = os.Chmod(l.StateDir, 0o777)
		if err := newPub(t, l, "x", clk).Start(context.Background()); err == nil {
			t.Fatal("started below a world-writable state directory")
		}
	})
	t.Run("world-writable ancestor", func(t *testing.T) {
		base := t.TempDir()
		anc := filepath.Join(base, "a")
		_ = os.Mkdir(anc, 0o755)
		_ = os.Chmod(anc, 0o777)
		state := filepath.Join(anc, "s")
		_ = os.Mkdir(state, 0o700)
		l := Layout{Scope: ScopeUser, UID: me(), StateDir: state, PrivateDir: filepath.Join(state, "presence")}
		if err := newPub(t, l, "x", clk).Start(context.Background()); err == nil {
			t.Fatal("started below a world-writable ancestor")
		}
	})
	t.Run("symlinked ancestor is resolved and checked", func(t *testing.T) {
		l := userLayout(t)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(l.StateDir, link); err != nil {
			t.Fatal(err)
		}
		l2 := l
		l2.PrivateDir = filepath.Join(link, "presence")
		start(t, newPub(t, l2, "viaLink", clk))
		if _, err := os.Stat(filepath.Join(l.PrivateDir, "viaLink.json")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("existing wide presence dir is tightened, state dir untouched", func(t *testing.T) {
		l := userLayout(t)
		_ = os.Mkdir(l.PrivateDir, 0o755)
		_ = os.Chmod(l.PrivateDir, 0o755)
		_ = os.Chmod(l.StateDir, 0o750)
		start(t, newPub(t, l, "x", clk))
		if mode(t, l.PrivateDir) != 0o700 || mode(t, l.StateDir) != 0o750 {
			t.Fatalf("modes %o %o", mode(t, l.PrivateDir), mode(t, l.StateDir))
		}
	})
	t.Run("public root with a world-writable mode", func(t *testing.T) {
		pub := filepath.Join(t.TempDir(), "pub")
		_ = os.Mkdir(pub, 0o700)
		_ = os.Chmod(pub, 0o777)
		if err := newPub(t, rootLayout(t, pub, "/s"), "x", clk).Start(context.Background()); err == nil {
			t.Fatal("published into a world-writable directory")
		}
	})
	t.Run("public root owned by someone else", func(t *testing.T) {
		pub := filepath.Join(t.TempDir(), "pub")
		l := rootLayout(t, pub, "/s")
		l.RootUID = me() + 1 // the directory is ours, "root" is another uid
		l.UID = l.RootUID
		if err := newPub(t, l, "x", clk).Start(context.Background()); err == nil {
			t.Fatal("trusted a directory of another owner")
		}
	})
	t.Run("public staging dir is a symlink", func(t *testing.T) {
		pub := filepath.Join(t.TempDir(), "pub")
		_ = os.Mkdir(pub, 0o755)
		elsewhere := t.TempDir()
		_ = os.Symlink(elsewhere, filepath.Join(pub, ".stage"))
		// The link is moved aside, never followed.
		p := newPub(t, rootLayout(t, pub, "/s"), "x", clk)
		start(t, p)
		if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
			t.Fatalf("staged through a symlink: %v", ents)
		}
		if fi, err := os.Lstat(filepath.Join(pub, ".stage")); err != nil || !fi.IsDir() {
			t.Fatalf("no fresh staging directory: %v", err)
		}
	})
}

func TestReaderRejectsHostileFiles(t *testing.T) {
	clk := newClock()
	l := userLayout(t)
	p := newPub(t, l, "good", clk)
	start(t, p)
	r := privReader(l, clk)
	good, _ := os.ReadFile(filepath.Join(l.PrivateDir, "good.json"))
	put := func(name string, data []byte, m os.FileMode) string {
		p := filepath.Join(l.PrivateDir, name)
		if err := os.WriteFile(p, data, m); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, m)
		return p
	}
	check := func(id string) Result {
		return r.Check(Candidate{Source: SourcePrivate, Dir: l.PrivateDir, Instance: id})
	}

	put("wide.json", good, 0o644)
	if res := check("wide"); res.Status != StatusUnavailable {
		t.Errorf("world-readable private record: %+v", res)
	}
	_ = os.Symlink(filepath.Join(l.PrivateDir, "good.json"), filepath.Join(l.PrivateDir, "link.json"))
	if res := check("link"); res.Status != StatusUnavailable {
		t.Errorf("symlink: %+v", res)
	}
	_ = os.Mkdir(filepath.Join(l.PrivateDir, "dir.json"), 0o700)
	if res := check("dir"); res.Status != StatusUnavailable {
		t.Errorf("directory: %+v", res)
	}
	if err := syscall.Mkfifo(filepath.Join(l.PrivateDir, "fifo.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan Result, 1)
	go func() { done <- check("fifo") }()
	select {
	case res := <-done:
		if res.Status != StatusUnavailable {
			t.Errorf("fifo: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a FIFO blocked the reader")
	}
	put("big.json", append(append([]byte(nil), good[:len(good)-1]...), append([]byte(`,"x":"`), append(make([]byte, MaxRecordSize), []byte(`"}`)...)...)...), 0o600)
	if res := check("big"); res.Status != StatusUnavailable {
		t.Errorf("oversize: %+v", res)
	}
	put("owner.json", []byte(`{"schema":"minion.presence/1","instance":"owner","daemon_version":"v","started_at":1,"updated_at":2,"stale_after_s":90,"state":"running","os_scope":"user","owner":"alice"}`), 0o600)
	if res := check("owner"); res.Status != StatusUnavailable {
		t.Errorf("extra field: %+v", res)
	}
	var rec Record
	_ = json.Unmarshal(good, &rec)
	rec.Instance = "future"
	rec.UpdatedAt = ms(clk.Now().Add(time.Minute))
	b, _ := json.Marshal(rec)
	put("future.json", b, 0o600)
	if res := check("future"); res.Status != StatusUnavailable || !errors.Is(res.Err, errFuture) {
		t.Errorf("future: %+v", res)
	}
	rec.Instance, rec.UpdatedAt, rec.OSScope = "scope", ms(clk.Now()), ScopeRoot
	b, _ = json.Marshal(rec)
	put("scope.json", b, 0o600)
	if res := check("scope"); res.Status != StatusUnavailable {
		t.Errorf("a root record in a private directory: %+v", res)
	}
	rec.Instance, rec.OSScope = "mismatch", ScopeUser
	b, _ = json.Marshal(rec)
	put("other.json", b, 0o600)
	if res := check("other"); res.Status != StatusUnavailable {
		t.Errorf("instance differs from the file name: %+v", res)
	}
	if res := check("../good"); res.Status != StatusUnavailable {
		t.Errorf("path traversal: %+v", res)
	}
	// The good one still reads, and bad candidates never hide it.
	if got := Best(r.Scan()); got.Status != StatusRunning || got.Record.Instance != "good" {
		t.Errorf("best: %+v", got)
	}
}

func TestPublicReaderRejectsWrongModeOwnerAndLinks(t *testing.T) {
	clk := newClock()
	pub := filepath.Join(t.TempDir(), "pub")
	l := rootLayout(t, pub, "/s")
	start(t, newPub(t, l, "r1", clk))
	r := pubReader(l, clk)
	good, _ := os.ReadFile(filepath.Join(pub, "r1.json"))
	for name, m := range map[string]os.FileMode{"w": 0o666, "p": 0o600} {
		p := filepath.Join(pub, name+".json")
		_ = os.WriteFile(p, good, m)
		_ = os.Chmod(p, m)
		if res := r.Check(Candidate{Source: SourcePublic, Dir: pub, Instance: name}); res.Status != StatusUnavailable {
			t.Errorf("mode %o accepted: %+v", m, res)
		}
	}
	// A reader that expects another root owner refuses everything.
	other := r
	other.RootUID = me() + 1
	if res := other.Scan(); len(res) != 1 || res[0].Status != StatusUnavailable {
		t.Errorf("foreign owner: %+v", res)
	}
	// A private record must never be accepted as public and vice versa.
	if res := (Reader{UID: me(), PrivateDirs: []string{pub}, Now: clk.Now}).Scan(); len(res) > 0 && res[0].Status == StatusRunning {
		t.Errorf("a 0755 directory was accepted as private: %+v", res)
	}
}

func TestPrivateAndPublicAreEnumeratedIndependently(t *testing.T) {
	clk := newClock()
	u := userLayout(t)
	pub := filepath.Join(t.TempDir(), "pub")
	rl := rootLayout(t, pub, "/s")
	pu := newPub(t, u, "stale-user", clk)
	start(t, pu)
	start(t, newPub(t, rl, "live-root", clk))
	clk.Add(10 * time.Minute) // the user record is stale now
	// Re-publish the root one (as its heartbeat would).
	rp := newPub(t, rl, "live-root", clk)
	rp.startedAt = clk.Now()
	if err := rp.b.write("live-root.json", rp.record(StateRunning, clk.Now())); err != nil {
		t.Fatal(err)
	}
	r := Reader{UID: me(), RootUID: me(), PrivateDirs: []string{u.PrivateDir}, PublicRoot: pub, Now: clk.Now}
	res := r.Scan()
	if len(res) != 2 || res[0].Candidate.Source != SourcePublic || res[0].Status != StatusRunning || res[1].Status != StatusStale {
		t.Fatalf("a stale private record must not hide the live public one: %+v", res)
	}
	// And the other way round: a broken public directory does not hide the private record.
	r.PublicRoot = filepath.Join(t.TempDir(), "missing")
	r.PrivateDirs = []string{u.PrivateDir}
	if res := r.Scan(); len(res) != 1 || res[0].Status != StatusStale {
		t.Fatalf("%+v", res)
	}
}

func TestConcurrentReadersDuringReplacement(t *testing.T) {
	for _, public := range []bool{false, true} {
		clk := newClock()
		var l Layout
		var r Reader
		if public {
			l = rootLayout(t, filepath.Join(t.TempDir(), "pub"), "/s")
			r = pubReader(l, clk)
		} else {
			l = userLayout(t)
			r = privReader(l, clk)
		}
		p := newPub(t, l, "conc", clk)
		start(t, p)
		var bad atomic.Int64
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					res := r.Scan()
					if len(res) != 1 || res[0].Status != StatusRunning {
						bad.Add(1)
					}
					if _, err := r.ReadUsage(Candidate{Source: res[0].Candidate.Source, Dir: res[0].Candidate.Dir, Instance: "conc"}); err != nil && !errors.Is(err, os.ErrNotExist) {
						bad.Add(1)
					}
				}
			}()
		}
		doc := []byte(usageDoc(0))
		for i := 0; i < 150; i++ {
			clk.Add(time.Second)
			p.heartbeat()
			if err := p.SetUsage(doc); err != nil {
				t.Fatal(err)
			}
		}
		close(stop)
		wg.Wait()
		if bad.Load() != 0 {
			t.Fatalf("public=%v: %d reads saw a torn or missing record", public, bad.Load())
		}
	}
}

func TestStartupFailureLeavesNothing(t *testing.T) {
	clk := newClock()
	// The record cannot be written: the state directory vanishes underneath.
	l := userLayout(t)
	_ = os.Chmod(l.StateDir, 0o777)
	p := newPub(t, l, "fail", clk)
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := os.Stat(l.PrivateDir); err == nil {
		ents, _ := os.ReadDir(l.PrivateDir)
		if len(ents) != 0 {
			t.Fatalf("left %v", ents)
		}
	}
	if err := p.Stop(); err != nil { // a failed start has nothing to stop
		t.Fatal(err)
	}
	if err := p.SetUsage([]byte(usageDoc(1))); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("SetUsage on a stopped publisher: %v", err)
	}
	// Failure to claim in a root layout: unsupported platform/scope combos fail in New.
	bad := rootLayout(t, "relative/path", "/s")
	if _, err := New(Options{Layout: bad, Instance: "x", Version: "v"}); err == nil {
		t.Fatal("relative public root accepted")
	}
	if _, err := New(Options{Layout: userLayout(t), Instance: "../x", Version: "v"}); err == nil {
		t.Fatal("bad instance accepted")
	}
}

func TestReplacementAndLateStopMarker(t *testing.T) {
	for _, public := range []bool{false, true} {
		clk := newClock()
		var l Layout
		var r Reader
		if public {
			l = rootLayout(t, filepath.Join(t.TempDir(), "pub"), "/state/p")
			r = pubReader(l, clk)
		} else {
			l = userLayout(t)
			r = privReader(l, clk)
		}
		a := newPub(t, l, "old-1", clk)
		start(t, a)
		if err := a.SetUsage([]byte(usageDoc(1))); err != nil {
			t.Fatal(err)
		}
		b := newPub(t, l, "new-2", clk)
		start(t, b)
		// The successor removed the predecessor's record and usage document.
		res := r.Scan()
		if len(res) != 1 || res[0].Candidate.Instance != "new-2" {
			t.Fatalf("public=%v: %+v", public, res)
		}
		// The old daemon stops late: no stopped marker, no heartbeat appears.
		a.heartbeat()
		if err := a.Stop(); err != nil {
			t.Fatal(err)
		}
		res = r.Scan()
		if len(res) != 1 || res[0].Candidate.Instance != "new-2" || res[0].Status != StatusRunning {
			t.Fatalf("public=%v after late stop: %+v", public, res)
		}
		if err := b.Stop(); err != nil {
			t.Fatal(err)
		}
		if res := r.Scan(); len(res) != 1 || res[0].Status != StatusStopped {
			t.Fatalf("%+v", res)
		}
	}
}

func TestUsageDocumentStoredAndCleared(t *testing.T) {
	for _, public := range []bool{false, true} {
		withUmask(t, 0o077)
		clk := newClock()
		var l Layout
		var r Reader
		var dir string
		var want os.FileMode
		if public {
			dir = filepath.Join(t.TempDir(), "pub")
			l, want = rootLayout(t, dir, "/s"), 0o644
			r = pubReader(l, clk)
		} else {
			l, want = userLayout(t), 0o600
			dir, r = l.PrivateDir, privReader(l, clk)
		}
		p := newPub(t, l, "u1", clk)
		start(t, p)
		doc := []byte(usageDoc(7))
		if err := p.SetUsage(doc); err != nil {
			t.Fatal(err)
		}
		if err := p.SetUsage([]byte(`{"schema":"other"}`)); err == nil {
			t.Fatal("a foreign document was stored")
		}
		up := filepath.Join(dir, "u1.usage.json")
		if m := mode(t, up); m != want {
			t.Fatalf("public=%v usage mode %o", public, m)
		}
		res := Best(r.Scan())
		got, err := r.ReadUsage(res.Candidate)
		if err != nil || string(got) != string(doc) || string(p.Usage()) != string(doc) {
			t.Fatalf("read back: %q %v", got, err)
		}
		// The usage file is not a presence candidate of its own.
		if n := len(r.Scan()); n != 1 {
			t.Fatalf("%d candidates", n)
		}
		if err := p.ClearUsage(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(up); !os.IsNotExist(err) || p.Usage() != nil {
			t.Fatal("usage not cleared from disk and memory")
		}
		if _, err := r.ReadUsage(res.Candidate); err == nil {
			t.Fatal("usage still readable")
		}
	}
}

func TestResetClearsEverythingAndNothingResurrects(t *testing.T) {
	for _, public := range []bool{false, true} {
		clk := newClock()
		var l Layout
		var r Reader
		if public {
			l = rootLayout(t, filepath.Join(t.TempDir(), "pub"), "/state/p")
			r = pubReader(l, clk)
			// An unrelated root profile in the same directory must survive.
			other := newPub(t, rootLayout(t, l.PublicRoot, "/state/other"), "other-1", clk)
			start(t, other)
		} else {
			l = userLayout(t)
			l.DiscoveryDir = filepath.Join(t.TempDir(), "idx")
			r = privReader(l, clk)
		}
		p := newPub(t, l, "r1", clk)
		start(t, p)
		_ = p.SetUsage([]byte(usageDoc(1)))
		if err := ClearProfile(l); err != nil {
			t.Fatal(err)
		}
		for _, res := range r.Scan() {
			if res.Candidate.Instance == "r1" {
				t.Fatalf("public=%v: record survived reset: %+v", public, res)
			}
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(map[bool]string{true: l.PublicRoot + "/x", false: l.PrivateDir + "/x"}[public]), "r1.usage.json")); err == nil {
			t.Fatal("usage survived reset")
		}
		p.heartbeat()
		if err := p.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := p.SetUsage([]byte(usageDoc(1))); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("SetUsage after reset: %v", err)
		}
		for _, res := range r.Scan() {
			if res.Candidate.Instance == "r1" {
				t.Fatalf("public=%v: the daemon resurrected its record: %+v", public, res)
			}
		}
		if public {
			if got := Best(r.Scan()); got.Record == nil || got.Record.Instance != "other-1" {
				t.Fatalf("the other profile was touched: %+v", got)
			}
		}
		// Clearing twice, and a profile that never published, are fine.
		if err := ClearProfile(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := ClearProfile(userLayout(t)); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredEntriesAreCleaned(t *testing.T) {
	clk := newClock()
	pub := filepath.Join(t.TempDir(), "pub")
	l := rootLayout(t, pub, "/state/a")
	old := newPub(t, rootLayout(t, pub, "/state/old"), "crashed", clk)
	start(t, old)
	_ = old.SetUsage([]byte(usageDoc(1)))
	live := newPub(t, rootLayout(t, pub, "/state/live"), "alive", clk)
	start(t, live)
	age := func(name string, d time.Duration) {
		tm := clk.Now().Add(-d)
		_ = os.Chtimes(filepath.Join(pub, name), tm, tm)
	}
	age("crashed.usage.json", 2*time.Hour)
	// Make "crashed" look old by its content too.
	clk.Add(2 * time.Hour)
	if err := live.b.write("alive.json", live.record(StateRunning, clk.Now())); err != nil {
		t.Fatal(err)
	}
	// A hand-made orphan usage file of an unknown instance, old.
	_ = os.WriteFile(filepath.Join(pub, "ghost.usage.json"), []byte(`{}`), 0o644)
	_ = os.Chmod(filepath.Join(pub, "ghost.usage.json"), 0o644)
	age("ghost.usage.json", 3*time.Hour)

	start(t, newPub(t, l, "fresh", clk))
	var names []string
	ents, _ := os.ReadDir(pub)
	for _, e := range ents {
		names = append(names, e.Name())
	}
	has := func(n string) bool {
		_, err := os.Lstat(filepath.Join(pub, n))
		return err == nil
	}
	if has("crashed.json") || has("crashed.usage.json") || has("ghost.usage.json") {
		t.Fatalf("expired entries remain: %v", names)
	}
	if !has("alive.json") || !has("fresh.json") {
		t.Fatalf("live entries were removed: %v", names)
	}
}

func TestStageTempsAreSwept(t *testing.T) {
	clk := newClock()
	pub := filepath.Join(t.TempDir(), "pub")
	l := rootLayout(t, pub, "/s")
	start(t, newPub(t, l, "s1", clk))
	tmp := filepath.Join(pub, ".stage", "tmp-abandoned")
	_ = os.WriteFile(tmp, []byte("x"), 0o600)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(tmp, old, old)
	start(t, newPub(t, l, "s2", clk, func(o *Options) { o.Now = time.Now }))
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("abandoned staging file kept")
	}
}
