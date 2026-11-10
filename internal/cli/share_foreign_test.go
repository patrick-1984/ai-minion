package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// share_get and share_set name the Worker they run on in worker_id. Without a
// Hub nothing can route to another Worker, and the command is not sent to the
// local Worker instead: a named Worker is refused before anything is sent, and
// the refusal says what to do. The local Worker holds the same line for a caller
// that does not come through this command (see the Worker's share tests).
func TestShareCommandsDoNotSendANamedWorkerToTheLocalSocket(t *testing.T) {
	const foreign = "01900000-0000-7000-8000-0000000000ff"
	for _, args := range [][]string{
		{"share_set", "--worker-id", foreign, "--mode", "all"},
		{"share_get", "--worker-id", foreign},
	} {
		b := &fakeOpsBackend{call: answerWith(json.RawMessage(`{"rule":{"mode":"all"},"unshared":[]}`), nil)}
		code, out, errs := runWith(t, testDeps(b), "", args...)
		if b.count() != 0 {
			t.Fatalf("%v: a named Worker reached the local backend: exit=%d out=%s err=%s args=%s", args, code, out, errs, b.last(t).Args)
		}
		if code != ExitUsage || !strings.Contains(errs, "--worker-id") || !strings.Contains(errs, foreign) {
			t.Fatalf("%v: exit %d, error %q: the refusal must be a usage error that names --worker-id", args, code, errs)
		}
	}
}
