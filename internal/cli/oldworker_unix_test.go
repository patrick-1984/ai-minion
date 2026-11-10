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
