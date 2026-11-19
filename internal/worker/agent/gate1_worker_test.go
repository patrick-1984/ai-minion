package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Gate 1: worker_stop admits only the instance and the enrollment generation
// the caller read.
func TestStopOpGuardsInstanceAndEpoch(t *testing.T) {
	dir := t.TempDir()
	var stops atomic.Int32
	l := localExecutor{instance: "i1", stateDir: dir, stop: func() { stops.Add(1) }}
	call := func(a any) error {
		raw, _ := json.Marshal(a)
		_, err := localStopOp(l, context.Background(), localapi.Request{Op: OpLocalStop, Args: raw})
		return err
	}
	wantAPIError(t, call(LocalStopArgs{Instance: "other"}), "conflict", "state_changed")
	five := uint64(5)
	wantAPIError(t, call(LocalStopArgs{Instance: "i1", EnrollmentEpoch: &five}), "conflict", "state_changed")
	time.Sleep(300 * time.Millisecond)
	if stops.Load() != 0 {
		t.Fatal("a refused stop stopped the Worker")
	}
	zero := uint64(0)
	if err := call(LocalStopArgs{Instance: "i1", EnrollmentEpoch: &zero}); err != nil {
		t.Fatal(err)
	}
	// No arguments (an older client): the bare stop still works.
	if _, err := localStopOp(l, context.Background(), localapi.Request{Op: OpLocalStop}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if stops.Load() != 2 {
		t.Fatalf("stops = %d, want 2", stops.Load())
	}
}

func TestLifecycleChecksEnrollmentEpoch(t *testing.T) {
	e := newEnv(t)
	s, stops, _ := lifecycleSvc(t, e, SessionCounter{Host: e.host}, RunModeForeground)
	s.stateDir = t.TempDir()
	five := uint64(5)
	_, err := lifecycleCall(s, LocalLifecycleArgs{Action: LifecycleStop, Instance: "i1", EnrollmentEpoch: &five})
	wantAPIError(t, err, "conflict", "state_changed")
	time.Sleep(300 * time.Millisecond)
	if stops.Load() != 0 {
		t.Fatal("a refused stop stopped the Worker")
	}
}

func TestShareApplyChecksInstance(t *testing.T) {
	e := newEnv(t)
	s := shareLocal{st: e.st, ex: e.ex, policy: e.policy, instance: "i1", overriddenBy: []string{}}
	_, err := s.applyOp(context.Background(), mustJSON(t, LocalShareApplyArgs{Mode: "none", Instance: "i2"}))
	wantAPIError(t, err, "conflict", "state_changed")
	if st, _ := e.st.LoadShareStateContext(context.Background()); st.Mode != store.ShareAll {
		t.Fatalf("a refused apply changed the rule: %v", st.Mode)
	}
	if _, err := s.applyOp(context.Background(), mustJSON(t, LocalShareApplyArgs{Mode: "none", Instance: "i1"})); err != nil {
		t.Fatal(err)
	}
}

func shareSettings(t *testing.T, fileVal string, env map[string]string) Settings {
	t.Helper()
	dir := t.TempDir()
	if fileVal != "" {
		writeShareConfig(t, dir, map[string]string{config.KeyShareExisting: fileVal})
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	return Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, ShareExisting: "all"}, // the stale value read at process start
		Resolver:   &config.Resolver{Role: config.ScopeWorker, LookupEnv: lookup},
	}
}

// The rule is resolved again at start: a file that lost share_existing since
// the process read its config gives no stale "all".
func TestRereadShareExisting(t *testing.T) {
	s := shareSettings(t, "", nil)
	writeShareConfig(t, s.StateDir, map[string]string{config.KeyName: "w"}) // a file that no longer holds the rule
	if v, launch := RereadShareExisting(s); v != "" || launch {
		t.Fatalf("removed from the file: %q %v", v, launch)
	}
	s = shareSettings(t, "none", nil)
	if v, launch := RereadShareExisting(s); v != "none" || launch {
		t.Fatalf("file: %q %v", v, launch)
	}
	s = shareSettings(t, "none", map[string]string{"MINION_SHARE_EXISTING": "all"})
	if v, launch := RereadShareExisting(s); v != "all" || !launch {
		t.Fatalf("env: %q %v", v, launch)
	}
	// An unreadable file never widens.
	s = shareSettings(t, "", nil)
	if err := os.WriteFile(filepath.Join(s.StateDir, WorkerConfigFile), []byte("{not valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, _ := RereadShareExisting(s); v != "" {
		t.Fatalf("unreadable file: %q", v)
	}
}

func TestLaunchRuleNeverWidensSavedChoice(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareNone, Configured: true}); err != nil {
		t.Fatal(err)
	}
	sel, refused, err := applyShareExisting(ctx, e.st, "all", true)
	if err != nil || !refused || sel.Mode != store.ShareNone {
		t.Fatalf("launch all over none: %+v refused=%v %v", sel, refused, err)
	}
	if got, _ := e.st.LoadShareContext(ctx); got.Mode != store.ShareNone {
		t.Fatalf("saved choice widened to %v", got.Mode)
	}
	// A rule from the file applies as before.
	if _, refused, err = applyShareExisting(ctx, e.st, "all", false); err != nil || refused {
		t.Fatalf("file rule: refused=%v %v", refused, err)
	}
	// A launch rule that narrows applies.
	if sel, refused, err = applyShareExisting(ctx, e.st, "none", true); err != nil || refused || sel.Mode != store.ShareNone {
		t.Fatalf("launch none: %+v %v %v", sel, refused, err)
	}
}

func TestShareListReportsRefusedLaunchRule(t *testing.T) {
	e := newEnv(t)
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareNone, Configured: true}); err != nil {
		t.Fatal(err)
	}
	s := shareLocal{st: e.st, ex: e.ex, policy: e.policy, overriddenBy: []string{"env"}, launchRule: "all"}
	r := shareListOf(t, func() (json.RawMessage, error) { return s.listOp(context.Background()) })
	if r.LaunchRuleRefused != "all" || len(r.OverriddenBy) != 0 {
		t.Fatalf("list = %+v", r)
	}
	s.launchRule = "none"
	r = shareListOf(t, func() (json.RawMessage, error) { return s.listOp(context.Background()) })
	if r.LaunchRuleRefused != "" || !slices.Equal(r.OverriddenBy, []string{"env"}) {
		t.Fatalf("narrowing rule: %+v", r)
	}
}

// BuildStatus reports the running Worker's own block and its sources, and
// unknown when a Worker that runs cannot say.
func TestBuildStatusTakesTheDaemonsNewSessionsBlock(t *testing.T) {
	yes, no := true, false
	shell := &config.Tightened{} // this shell resolves "not blocked"
	rep := BuildStatus(context.Background(), StatusInputs{NewSessions: shell,
		Live: fixedLive{LiveStatus{NewSessionsBlocked: &yes, NewSessionsSources: []string{"env", "file"}}}})
	ns := rep.NewSessions
	if !ns.Blocked || !slices.Equal(ns.Sources, []string{"env", "file"}) || ns.InForce == nil || !*ns.InForce {
		t.Fatalf("blocked daemon: %+v", ns)
	}
	// An older daemon that blocks but names no source.
	rep = BuildStatus(context.Background(), StatusInputs{NewSessions: shell,
		Live: fixedLive{LiveStatus{NewSessionsBlocked: &yes}}})
	if !slices.Equal(rep.NewSessions.Sources, []string{"worker"}) {
		t.Fatalf("sources = %v", rep.NewSessions.Sources)
	}
	blockedShell := &config.Tightened{Blocked: true, Sources: []config.Source{config.SourceFile}}
	rep = BuildStatus(context.Background(), StatusInputs{NewSessions: blockedShell,
		Live: fixedLive{LiveStatus{NewSessionsBlocked: &no}}})
	ns = rep.NewSessions
	if ns.Blocked || len(ns.Sources) != 0 || ns.InForce == nil || !*ns.InForce {
		t.Fatalf("unblocked daemon: %+v", ns)
	}
	// The reply carries the sources over the local socket.
	r := NewLiveStatusReply("d", LiveStatus{NewSessionsBlocked: &yes, NewSessionsSources: []string{"env"}})
	if back, err := r.LiveStatus(); err != nil || !slices.Equal(back.NewSessionsSources, []string{"env"}) {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func writeShareConfig(t *testing.T, dir string, kv map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := config.LoadFile(filepath.Join(dir, WorkerConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		f.Set(k, v)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
}
