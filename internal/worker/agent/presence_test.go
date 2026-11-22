//go:build !windows

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/presence"
)

// injectPresence makes the presence module and the reset hooks publish into a
// temporary root (public) or the profile's own directory (private).
func injectPresence(t *testing.T, public bool) (pubRoot string) {
	t.Helper()
	old := presenceLayout
	t.Cleanup(func() { presenceLayout = old })
	if !public {
		presenceLayout = func(state string) (presence.Layout, error) {
			return presence.Layout{Scope: presence.ScopeUser, UID: os.Geteuid(), StateDir: state, PrivateDir: filepath.Join(state, "presence")}, nil
		}
		return ""
	}
	pubRoot = filepath.Join(t.TempDir(), "pub")
	presenceLayout = func(state string) (presence.Layout, error) {
		return presence.Layout{Scope: presence.ScopeRoot, UID: os.Geteuid(), RootUID: os.Geteuid(), StateDir: state, PublicRoot: pubRoot}, nil
	}
	return pubRoot
}

func presenceRuntime(t *testing.T, state string, extra ...func(*Registry)) *Runtime {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register(ModulePresence, OrderPresence, startPresence); err != nil {
		t.Fatal(err)
	}
	for _, f := range extra {
		f(reg)
	}
	return NewRuntime(reg, Options{})
}

func scanOne(t *testing.T, l presence.Layout) presence.Result {
	t.Helper()
	r := presence.Reader{UID: l.UID, RootUID: l.RootUID, PublicRoot: l.PublicRoot}
	if l.Scope == presence.ScopeUser {
		r.PrivateDirs = []string{l.PrivateDir}
	}
	res := r.Scan()
	if len(res) != 1 {
		t.Fatalf("scan: %+v", res)
	}
	return res[0]
}

func TestPresenceModuleLifecycleMatchesSchema(t *testing.T) {
	schema := loadSchema(t, "presence.schema.json")
	for _, public := range []bool{false, true} {
		injectPresence(t, public)
		state := t.TempDir()
		rt := presenceRuntime(t, state)
		if err := rt.Start(context.Background(), Settings{WorkerCore: config.WorkerCore{StateDir: state}}); err != nil {
			t.Fatal(err)
		}
		l, _ := presenceLayout(state)
		res := scanOne(t, l)
		if res.Status != presence.StatusRunning || res.Record.Instance != rt.Instance() {
			t.Fatalf("public=%v: %+v %v", public, res, res.Err)
		}
		raw, err := os.ReadFile(filepath.Join(res.Candidate.Dir, rt.Instance()+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(decodeAny(t, raw)); err != nil {
			t.Fatalf("the record does not match minion.presence/1: %v\n%s", err, raw)
		}
		if _, ok := Lookup[*presence.Publisher](rt); !ok {
			t.Fatal("the publisher is not provided as a service")
		}
		if err := rt.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if res := scanOne(t, l); res.Status != presence.StatusStopped {
			t.Fatalf("after stop: %+v", res)
		}
	}
}

func TestPresenceFailureNeverKeepsTheWorkerFromStarting(t *testing.T) {
	old := presenceLayout
	defer func() { presenceLayout = old }()
	presenceLayout = func(state string) (presence.Layout, error) { return presence.Layout{}, errors.New("no layout") }
	state := t.TempDir()
	rt := presenceRuntime(t, state)
	if err := rt.Start(context.Background(), Settings{WorkerCore: config.WorkerCore{StateDir: state}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := Lookup[*presence.Publisher](rt); ok {
		t.Fatal("a publisher without a record was provided")
	}
	_ = rt.Stop(context.Background())
}

func TestPresenceSurvivesAFailingLaterModuleAsStoppedMarker(t *testing.T) {
	injectPresence(t, false)
	state := t.TempDir()
	rt := presenceRuntime(t, state, func(r *Registry) {
		_ = r.Register("zz-fails", OrderPresence+50, func(context.Context, *Env) (StopFunc, error) { return nil, errors.New("boom") })
	})
	if err := rt.Start(context.Background(), Settings{WorkerCore: config.WorkerCore{StateDir: state}}); err == nil {
		t.Fatal("expected a start failure")
	}
	l, _ := presenceLayout(state)
	if res := scanOne(t, l); res.Status != presence.StatusStopped {
		t.Fatalf("a failed start must leave a stopped marker, not a running record: %+v", res)
	}
}

func TestResetWorkerClearsPresenceAndUsage(t *testing.T) {
	for _, public := range []bool{false, true} {
		injectPresence(t, public)
		r := newResetRig(t)
		l, _ := presenceLayout(r.dir)
		pub, err := presence.New(presence.Options{Layout: l, Instance: "daemon-1", Version: "v1", Interval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if err := pub.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := pub.SetUsage([]byte(`{"schema":"minion.usage-meters/1","generated_at":1,"slots":[]}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.reset(); err != nil {
			t.Fatal(err)
		}
		ents, _ := os.ReadDir(pdir(l))
		for _, e := range ents {
			if e.Name() == "daemon-1.json" || e.Name() == "daemon-1.usage.json" {
				t.Fatalf("public=%v: %s survived the reset", public, e.Name())
			}
		}
		_ = pub.Stop()
		ents, _ = os.ReadDir(pdir(l))
		for _, e := range ents {
			if e.Name() == "daemon-1.json" {
				t.Fatal("the stopping daemon resurrected its record after a reset")
			}
		}
	}
}

func TestEnrollmentWipeClearsUsage(t *testing.T) {
	injectPresence(t, false)
	e, _, dir := newEnrollments(t)
	l, _ := presenceLayout(dir)
	pub, _ := presence.New(presence.Options{Layout: l, Instance: "d1", Version: "v1", Interval: time.Hour})
	if err := pub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pub.Stop() }()
	put := func() {
		if err := pub.SetUsage([]byte(`{"schema":"minion.usage-meters/1","generated_at":1,"slots":[]}`)); err != nil {
			t.Fatal(err)
		}
	}
	// Offline process (no hook): the usage file goes, the record stays.
	put()
	if _, err := e.Wipe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.PrivateDir, "d1.usage.json")); !os.IsNotExist(err) {
		t.Fatal("usage file survived an offline wipe")
	}
	if _, err := os.Stat(filepath.Join(l.PrivateDir, "d1.json")); err != nil {
		t.Fatalf("an offline wipe removed the running daemon's record: %v", err)
	}
	// Running daemon (hook): memory and disk are cleared through the publisher.
	e.SetWipeHook(func() { _ = pub.ClearUsage() })
	put()
	if _, err := e.Wipe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pub.Usage() != nil {
		t.Fatal("usage kept in memory")
	}
	if _, err := os.Stat(filepath.Join(l.PrivateDir, "d1.usage.json")); !os.IsNotExist(err) {
		t.Fatal("usage file survived")
	}
}

func pdir(l presence.Layout) string {
	if l.Scope == presence.ScopeRoot {
		return l.PublicRoot
	}
	return l.PrivateDir
}
