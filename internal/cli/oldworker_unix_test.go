//go:build unix

package cli

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/oldlocal"
)

// TestCLIAgainstAnOlderWorkerDaemon: the current CLI on the real private socket
// of a daemon that predates the safeguards. A reviewed answer and a submitting or
// multi-line send_text are refused before anything reaches the daemon (exit 1, no
// outcome printed as delivered), with the update-needed message; what an older
// Worker handles the same way still works.
func TestCLIAgainstAnOlderWorkerDaemon(t *testing.T) {
	d := oldlocal.New(t)
	deps := testDeps(&fakeOpsBackend{})
	deps.newID = ops.NewUUIDv7
	deps.local = func() client.Backend { return d.Backend }

	for _, args := range [][]string{
		{"answer", "--target", "work", "--choice", "1", "--expect", reviewedJSON},
		{"send_text", "--target", "work", "--text", "hello", "--submit"},
		{"send_text", "--target", "work", "--text", "a\nb"},
	} {
		code, out, errs := runWith(t, deps, "", args...)
		if code != ExitError || strings.Contains(out, "delivered") || !strings.Contains(errs, "older than this client") || !strings.Contains(errs, "Nothing was sent") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errs)
		}
	}
	if runs := d.Runs(); len(runs) != 0 {
		t.Fatalf("the older daemon ran %d guarded requests: %+v", len(runs), runs)
	}
	if code, out, errs := runWith(t, deps, "", "send_text", "--target", "work", "--text", "plain line"); code != ExitOK || !strings.Contains(out, "delivered") {
		t.Fatalf("a plain text: exit %d, %q, %s", code, out, errs)
	}
	if runs := d.Runs(); len(runs) != 1 || runs[0].Op != ops.OpSendText {
		t.Fatalf("runs = %+v, want the plain text only", runs)
	}
}

// TestCLIForeignTargetAgainstAnOlderWorkerDaemon: a session-control request that
// names another Worker goes to the local Worker, which forwards it to its Hub
// (SC-5). A daemon that predates the forward ignores the Worker part and would
// act on a same-named local session, so the request requires the feature
// session.forward and the older daemon refuses it: nothing reaches its terminal.
func TestCLIForeignTargetAgainstAnOlderWorkerDaemon(t *testing.T) {
	d := oldlocal.New(t)
	deps := depsWithSettings(&fakeOpsBackend{}, map[string]string{}) // reads --hub-worker; no Hub URL
	deps.newID = ops.NewUUIDv7
	deps.local = func() client.Backend { return d.Backend }
	foreign := ops.NewUUIDv7()
	for _, args := range [][]string{
		{"send_text", "--target", foreign + ":work", "--text", "plain line"},
		{"send_keys", "--target", foreign + ":work", "--keys", "Enter"},
		{"session_snapshot", "--target", foreign + ":work"},
		{"sessions_list", "--hub-worker", foreign},
	} {
		code, out, errs := runWith(t, deps, "", args...)
		if code != ExitError || strings.Contains(out, "delivered") || !strings.Contains(errs, "older than this client") || !strings.Contains(errs, "session.forward") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errs)
		}
	}
	if runs := d.Runs(); len(runs) != 0 {
		t.Fatalf("the older daemon ran %d foreign-target requests: %+v", len(runs), runs)
	}
	// A target on the local Worker still works there.
	if code, out, errs := runWith(t, deps, "", "send_text", "--target", "work", "--text", "plain line"); code != ExitOK || !strings.Contains(out, "delivered") {
		t.Fatalf("a local target: exit %d, %q, %s", code, out, errs)
	}
}
