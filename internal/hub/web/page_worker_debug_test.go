package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// debugInfo is alice's Worker w1 that serves remote debug and allows it.
func debugInfo(allowed bool) ops.WorkerInfo {
	w := wAliceInfo
	w.Ops = []string{ops.OpWorkerDebug, ops.OpWorkerLogs}
	w.DebugRemote = allowed
	return w
}

func debugServer(t *testing.T, w ops.WorkerInfo, mutate func(f *fakeOps)) (*Server, *fakeOps) {
	t.Helper()
	d := Dashboard{Workers: []WorkerRow{wPrivate}}
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w}}).
		reply(ops.OpWorkerLogs, ops.WorkerLogsResult{
			WorkerDebugState: ops.WorkerDebugState{Debug: true, Remote: true, Level: "debug", RemainingSeconds: 1800},
			Lines:            []string{`level=DEBUG msg="terminal host listing" sessions=0`, `level=INFO msg=<b>x</b>`},
		})
	if mutate != nil {
		mutate(f)
	}
	connect(t, f)
	s, _ := listServer(t, d, d)
	return s, f
}

func TestDebugPanelExplainsWhyItCannotBeUsed(t *testing.T) {
	old := wAliceInfo
	offline := debugInfo(true)
	offline.Online = false
	for name, tc := range map[string]struct {
		w    ops.WorkerInfo
		want []string
	}{
		"not allowed": {debugInfo(false), []string{"does not allow remote debug", "--allow-debug-remote", "minion worker config set allow_debug_remote true"}},
		"too old":     {old, []string{"too old for remote debug"}},
		"offline":     {offline, []string{"This worker is offline"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := debugServer(t, tc.w, nil)
			rec := post(t, s, "/w/w1/debug", "alice:user", url.Values{"action": {"logs"}})
			if rec.Code != 200 {
				t.Fatalf("POST = %d", rec.Code)
			}
			mustHave(t, rec.Body.String(), append([]string{"data-debug-note"}, tc.want...)...)
			mustNotHave(t, rec.Body.String(), `data-control="debug-on"`, "data-debug-log")
			if len(f.called(ops.OpWorkerLogs)) != 0 {
				t.Fatal("the Worker was asked although it cannot serve")
			}
		})
	}
	t.Run("not the owner", func(t *testing.T) {
		d := Dashboard{Workers: []WorkerRow{wPrivate}}
		bob := wBobInfo
		bob.Online, bob.DebugRemote, bob.Ops = true, true, []string{ops.OpWorkerDebug, ops.OpWorkerLogs}
		f := newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{bob}})
		connect(t, f)
		s, _ := listServer(t, d, d)
		rec := post(t, s, "/w/w2/debug", "alice:user", url.Values{"action": {"logs"}})
		mustHave(t, rec.Body.String(), "Only the owner of this worker, or an administrator")
		if len(f.called(ops.OpWorkerLogs)) != 0 {
			t.Fatal("worker_logs was called for a person who may not operate the Worker")
		}
	})
}

// The page load reads nothing (each read is audited), and no GET does, also
// with the old open=1 query (a cross-site navigation must not read the log in
// the person's name); opening the panel is a CSRF-checked POST that shows the
// state and the escaped log.
func TestDebugPanelOpensWithStateAndLog(t *testing.T) {
	s, f := debugServer(t, debugInfo(true), nil)
	for _, path := range []string{"/w/w1/debug", "/w/w1/debug?open=1&lines=500"} {
		rec := get(t, s, path, "X-Test-Session", "alice:user")
		mustHave(t, rec.Body.String(), `data-control="debug-open"`, `hx-post="/w/w1/debug"`, `name="action" value="logs"`)
		mustNotHave(t, rec.Body.String(), "hx-get=")
	}
	if len(f.called(ops.OpWorkerLogs)) != 0 {
		t.Fatal("a GET read the log")
	}
	rec := post(t, s, "/w/w1/debug", "alice:user", url.Values{"action": {"logs"}, "lines": {"500"}})
	body := rec.Body.String()
	mustHave(t, body, `data-debug-state="on"`, "about 30 minutes", `data-control="debug-off"`, `data-control="debug-refresh"`,
		"terminal host listing", "msg=&lt;b&gt;x&lt;/b&gt;", `<option value="500" selected>`)
	mustNotHave(t, body, "<b>x</b>")
	calls := f.called(ops.OpWorkerLogs)
	if len(calls) != 1 || calls[0].Args["worker_id"] != "w1" || calls[0].Args["lines"] != float64(500) || calls[0].User != "alice" {
		t.Fatalf("worker_logs calls = %+v", calls)
	}
}

func TestDebugPostSwitchesAndShowsRefusals(t *testing.T) {
	s, f := debugServer(t, debugInfo(true), func(f *fakeOps) {
		f.reply(ops.OpWorkerDebug, ops.WorkerDebugState{Debug: true, Remote: true, Level: "debug", RemainingSeconds: 900})
	})
	rec := post(t, s, "/w/w1/debug", "alice:user", url.Values{"enabled": {"1"}, "minutes": {"15"}})
	mustHave(t, rec.Body.String(), "result-info", "Debug logging is on", "about 15 minutes")
	calls := f.called(ops.OpWorkerDebug)
	if len(calls) != 1 || calls[0].Args["enabled"] != true || calls[0].Args["minutes"] != float64(15) {
		t.Fatalf("worker_debug calls = %+v", calls)
	}
	rec = post(t, s, "/w/w1/debug", "alice:user", url.Values{"enabled": {"1"}, "minutes": {"5000"}})
	mustHave(t, rec.Body.String(), "result-danger", "Minutes must be 1 to 1440")
	if len(f.called(ops.OpWorkerDebug)) != 1 {
		t.Fatal("a bad duration was sent")
	}

	refused, _ := debugServer(t, debugInfo(true), func(f *fakeOps) {
		f.on(ops.OpWorkerDebug, func(map[string]any) (any, error) {
			return nil, &OpError{Status: 403, Code: "forbidden", Message: "this Worker does not allow remote debug (allow_debug_remote is off)."}
		})
	})
	rec = post(t, refused, "/w/w1/debug", "alice:user", url.Values{"enabled": {"0"}})
	mustHave(t, rec.Body.String(), "result-danger", "allow_debug_remote is off")
	mustNotHave(t, rec.Body.String(), "Debug logging is on")
}

// A switch that worked keeps its state and the off switch when the log read
// after it fails; only the log area reports the failure. With neither answer
// the panel offers both on and off.
func TestDebugSwitchKeepsControlsWhenTheLogReadFails(t *testing.T) {
	gone := func(map[string]any) (any, error) {
		return nil, &OpError{Status: 503, Code: "worker_offline", Message: "the Worker is offline"}
	}
	s, _ := debugServer(t, debugInfo(true), func(f *fakeOps) {
		f.reply(ops.OpWorkerDebug, ops.WorkerDebugState{Debug: true, Remote: true, Level: "debug", RemainingSeconds: 900})
		f.on(ops.OpWorkerLogs, gone)
	})
	rec := post(t, s, "/w/w1/debug", "alice:user", url.Values{"enabled": {"1"}, "minutes": {"15"}})
	body := rec.Body.String()
	mustHave(t, body, "Debug logging is on", `data-debug-state="on"`, `data-control="debug-off"`, `data-control="debug-refresh"`,
		"data-debug-log-error", "The log could not be read")
	mustNotHave(t, body, "data-debug-log>")

	both, _ := debugServer(t, debugInfo(true), func(f *fakeOps) {
		f.on(ops.OpWorkerDebug, gone)
		f.on(ops.OpWorkerLogs, gone)
	})
	rec = post(t, both, "/w/w1/debug", "alice:user", url.Values{"enabled": {"0"}})
	mustHave(t, rec.Body.String(), "result-danger", `data-debug-state="unknown"`, `data-control="debug-on"`, `data-control="debug-off"`)
}

func TestWorkerPageOffersTheDebugPanelToTheOwner(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}
	s, _ := workerPageServer(t, d, d, wAliceInfo)
	_, own := page(t, s, "/workers/w1", "alice:user")
	mustHave(t, own, `id="debug-panel"`, `hx-get="/w/w1/debug"`)
	if !strings.Contains(own, `id="debug-section"`) {
		t.Fatal("no debug section")
	}
}
