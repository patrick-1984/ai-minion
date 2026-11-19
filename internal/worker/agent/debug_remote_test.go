package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// fakeTimers records the auto-off timers and fires them on demand.
type fakeTimers struct {
	mu  sync.Mutex
	fns []func()
	dur []time.Duration
	off []bool
}

func (f *fakeTimers) after(d time.Duration, fn func()) func() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := len(f.fns)
	f.fns, f.dur, f.off = append(f.fns, fn), append(f.dur, d), append(f.off, false)
	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.off[i] = true
		return true
	}
}

func (f *fakeTimers) fire(i int) { f.fns[i]() }

func newTestDebug(t *testing.T, allowed bool) (*debugRemote, *fakeTimers, *slog.Logger, *time.Time) {
	t.Helper()
	log, live, err := logx.NewLive(logx.Options{Level: "info", Format: logx.FormatText, Output: &bytes.Buffer{}}, logx.NewRing(100, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	ft := &fakeTimers{}
	d := &debugRemote{log: log, allowed: allowed, live: live, workerID: func() string { return "w-self" },
		now: func() time.Time { return now }, afterFunc: ft.after}
	return d, ft, log, &now
}

func debugRequest(op string, args any) Request {
	raw, _ := json.Marshal(args)
	return Request{Op: op, Args: raw, Actions: []ops.Action{ops.ActionOperate},
		Origin: &proto.Origin{Kind: proto.OriginUser, UserID: "owner"}}
}

func TestDebugRemoteRefusedUnlessAllowed(t *testing.T) {
	d, _, _, _ := newTestDebug(t, false)
	for _, rq := range []Request{
		debugRequest(ops.OpWorkerDebug, ops.WorkerDebugArgs{WorkerID: "w-self", Enabled: true}),
		debugRequest(ops.OpWorkerLogs, ops.WorkerLogsArgs{WorkerID: "w-self"}),
	} {
		h := d.handleDebug
		if rq.Op == ops.OpWorkerLogs {
			h = d.handleLogs
		}
		_, err := h(context.Background(), rq)
		ae := apiErrorOf(t, err, string(proto.CodeForbidden), "")
		for _, want := range []string{"allow_debug_remote", "--allow-debug-remote", "minion worker config set allow_debug_remote true"} {
			if !strings.Contains(ae.Message, want) {
				t.Errorf("%s: message lacks %q: %s", rq.Op, want, ae.Message)
			}
		}
	}
	if d.live.Debug() {
		t.Fatal("a refused request switched debug on")
	}
}

func TestDebugRemoteRefusesLocalAndOtherWorker(t *testing.T) {
	d, _, _, _ := newTestDebug(t, true)
	local := debugRequest(ops.OpWorkerDebug, ops.WorkerDebugArgs{WorkerID: "w-self", Enabled: true})
	local.Origin = nil
	if _, err := d.handleDebug(context.Background(), local); err == nil {
		t.Fatal("a local request was served")
	}
	other := debugRequest(ops.OpWorkerLogs, ops.WorkerLogsArgs{WorkerID: "w-other"})
	if _, err := d.handleLogs(context.Background(), other); err == nil {
		t.Fatal("another Worker's id was served")
	}
	read := debugRequest(ops.OpWorkerDebug, ops.WorkerDebugArgs{WorkerID: "w-self", Enabled: true})
	read.Actions = []ops.Action{ops.ActionRead}
	if _, err := d.handleDebug(context.Background(), read); err == nil {
		t.Fatal("read alone switched debug on")
	}
	noLive := &debugRemote{allowed: true, workerID: func() string { return "w-self" }}
	if _, err := noLive.handleLogs(context.Background(), debugRequest(ops.OpWorkerLogs, ops.WorkerLogsArgs{WorkerID: "w-self"})); err == nil {
		t.Fatal("served without a log")
	}
}

func TestDebugRemoteOnAutoOffAndLogs(t *testing.T) {
	d, ft, log, now := newTestDebug(t, true)
	log.Debug("before")
	res, err := d.handleDebug(context.Background(), debugRequest(ops.OpWorkerDebug, ops.WorkerDebugArgs{WorkerID: "w-self", Enabled: true}))
	if err != nil {
		t.Fatal(err)
	}
	st := res.(ops.WorkerDebugState)
	if !st.Debug || !st.Remote || st.Level != "debug" || st.RemainingSeconds != 3600 || ft.dur[0] != time.Hour {
		t.Fatalf("state = %+v, timer %v", st, ft.dur)
	}
	log.Debug("during", "k", "v")
	*now = now.Add(10 * time.Minute)

	res, err = d.handleLogs(context.Background(), debugRequest(ops.OpWorkerLogs, ops.WorkerLogsArgs{WorkerID: "w-self", Lines: 2}))
	if err != nil {
		t.Fatal(err)
	}
	lr := res.(ops.WorkerLogsResult)
	if len(lr.Lines) != 2 || !strings.Contains(lr.Lines[1], "during") || lr.RemainingSeconds != 3000 || !lr.Remote {
		t.Fatalf("logs = %+v", lr)
	}
	for _, l := range lr.Lines {
		if strings.Contains(l, "before") {
			t.Fatalf("a debug line from before was kept: %q", l)
		}
	}

	// The time is up: debug switches itself off.
	ft.fire(0)
	if d.live.Debug() || d.state().Remote {
		t.Fatalf("still on after the timer: %+v", d.state())
	}
	log.Debug("after")
	if strings.Contains(strings.Join(d.live.Ring.Tail(0), "\n"), "after") {
		t.Fatal("debug line written after auto-off")
	}

	// A new request replaces the timer; the old one firing late changes nothing.
	if _, err := d.handleDebug(context.Background(), debugRequest(ops.OpWorkerDebug, ops.WorkerDebugArgs{WorkerID: "w-self", Enabled: true, Minutes: 5})); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	if _, err := d.handleDebug(context.Background(), debugRequest(ops.OpWorkerDebug, ops.WorkerDebugArgs{WorkerID: "w-self", Enabled: true, Minutes: 10})); err != nil {
		t.Fatal(err)
	}
	if !ft.off[1] || ft.dur[2] != 10*time.Minute {
		t.Fatalf("timers = %v %v", ft.dur, ft.off)
	}
	ft.fire(1)
	if !d.live.Debug() {
		t.Fatal("a replaced timer switched debug off")
	}
	// Off by request.
	res, err = d.handleDebug(context.Background(), debugRequest(ops.OpWorkerDebug, ops.WorkerDebugArgs{WorkerID: "w-self"}))
	if err != nil || res.(ops.WorkerDebugState).Debug || !ft.off[2] {
		t.Fatalf("off = %+v %v", res, err)
	}
}

func TestDebugRemoteBounds(t *testing.T) {
	d, _, _, _ := newTestDebug(t, true)
	for _, m := range []int{-1, ops.MaxDebugMinutes + 1} {
		_, err := d.handleDebug(context.Background(), debugRequest(ops.OpWorkerDebug, ops.WorkerDebugArgs{WorkerID: "w-self", Enabled: true, Minutes: m}))
		_ = apiErrorOf(t, err, string(proto.CodeInvalidArgument), "minutes")
	}
	for _, n := range []int{-1, ops.MaxLogLines + 1} {
		_, err := d.handleLogs(context.Background(), debugRequest(ops.OpWorkerLogs, ops.WorkerLogsArgs{WorkerID: "w-self", Lines: n}))
		_ = apiErrorOf(t, err, string(proto.CodeInvalidArgument), "lines")
	}
	// The answer stays below one frame however long the lines are.
	for range 2000 {
		d.live.Ring.Add(strings.Repeat(`"q"`, 1000))
	}
	res, err := d.handleLogs(context.Background(), debugRequest(ops.OpWorkerLogs, ops.WorkerLogsArgs{WorkerID: "w-self", Lines: ops.MaxLogLines}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if len(b) > proto.StreamFrameLimit-4096 || len(res.(ops.WorkerLogsResult).Lines) == 0 {
		t.Fatalf("answer = %d bytes", len(b))
	}
}

func TestSetupDebugRemoteSettingAndFact(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{{"", false}, {"1", true}, {"true", true}, {"bogus", false}} {
		env := map[string]string{}
		if tc.env != "" {
			env["MINION_ALLOW_DEBUG_REMOTE"] = tc.env
		}
		res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
		_, live, _ := logx.NewLive(logx.Options{Output: &bytes.Buffer{}}, nil)
		ex := &Executor{handlers: map[string]Handler{}}
		d := setupDebugRemote(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), Settings{Resolver: res, LogLive: live}, ex)
		if d.Allowed() != tc.want {
			t.Errorf("%q: allowed = %v", tc.env, d.Allowed())
		}
		facts := withDebugFact(func() transport.WorkerFacts { return transport.WorkerFacts{Build: "dev"} }, d)()
		if facts.DebugRemote != tc.want || facts.Build != "dev" {
			t.Errorf("%q: facts = %+v", tc.env, facts)
		}
		names := ex.Ops()
		if !strings.Contains(strings.Join(names, ","), ops.OpWorkerDebug) || !strings.Contains(strings.Join(names, ","), ops.OpWorkerLogs) {
			t.Errorf("ops = %v", names)
		}
	}
}

// Preview-only takes input to sessions away, not the owner's diagnostics:
// worker_debug and worker_logs keep operate, a session write does not.
func TestPreviewOnlyKeepsDebugOpsForTheOwner(t *testing.T) {
	p := NewPolicy(nil)
	p.SetHubPreviewOnly(true)
	s := newOpServer(t, &fakeExec{}, func(c *OpServerConfig) { c.Policy = p })
	for op, want := range map[string]bool{ops.OpWorkerDebug: true, ops.OpWorkerLogs: true, ops.OpSendKeys: false} {
		raw, _ := json.Marshal(opRequest(op, []ops.Action{ops.ActionOperate}, map[string]any{}))
		rq, _, perr := s.parse(raw)
		if perr != nil {
			t.Fatalf("%s: %v", op, perr)
		}
		if got := slices.Contains(rq.Actions, ops.ActionOperate); got != want {
			t.Errorf("%s on a preview-only Worker: operate kept = %v, want %v", op, got, want)
		}
	}
}

// A new owner or enrollment never reads the former one's log, and a debug
// period the former owner started ends.
func TestDebugRemoteResetsOnIdentityChange(t *testing.T) {
	d, ft, log, _ := newTestDebug(t, true)
	p := NewPolicy(nil)
	p.OnIdentityChange(d.identityChanged)
	p.SetWorkerID("w-self")
	p.SetOwnerID("alice")
	if _, err := d.handleDebug(context.Background(), debugRequest(ops.OpWorkerDebug, ops.WorkerDebugArgs{WorkerID: "w-self", Enabled: true})); err != nil {
		t.Fatal(err)
	}
	log.Info("alice's line")
	p.SetWorkerID("w-self") // the same id again: no change
	if !d.live.Debug() || !strings.Contains(strings.Join(d.live.Ring.Tail(0), "\n"), "alice's line") {
		t.Fatal("a repeated id reset the log")
	}
	p.SetOwnerID("bob")
	got := strings.Join(d.live.Ring.Tail(0), "\n")
	if d.live.Debug() || strings.Contains(got, "alice's line") || d.state().Remote {
		t.Fatalf("after the owner change: debug=%v ring=%q", d.live.Debug(), got)
	}
	if !ft.off[0] {
		t.Fatal("the auto-off timer was not cancelled")
	}
	log.Info("bob's line")
	p.SetWorkerID("")
	if strings.Contains(strings.Join(d.live.Ring.Tail(0), "\n"), "bob's line") {
		t.Fatal("the removal kept the log")
	}
}

// A Worker whose own traffic log records frame bodies says so in its hello,
// so that the Hub shows a notice on its sessions (A49 T19).
func TestTrafficPayloadFact(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  bool
	}{{"", false}, {"off", false}, {"metadata", false}, {"payload", true}, {"bogus", false}} {
		env := map[string]string{}
		if tc.level != "" {
			env["MINION_WORKER_TRAFFIC_LOG"] = tc.level
		}
		res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
		f := withTrafficFact(func() transport.WorkerFacts { return transport.WorkerFacts{Build: "dev"} }, Settings{Resolver: res})()
		if f.TrafficPayload != tc.want || f.Build != "dev" {
			t.Errorf("%q: facts = %+v", tc.level, f)
		}
	}
}
