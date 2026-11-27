package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/devctl/devctltest"
)

// Through the real control socket: the output streams as progress, the exit
// code comes back, and the audit names the session of the calling process,
// which the Worker found from the socket's peer pid (here the test process is
// the "pane" process of session build).
func TestAdbShellOverTheControlSocketMapsTheCallerSession(t *testing.T) {
	e, fake := adbEnv(t)
	e.host.mu.Lock()
	e.host.sessions[0].Panes[0].Ref.PID = os.Getpid()
	e.host.mu.Unlock()
	fake.Handle(func(string, string) devctltest.Result {
		return devctltest.Result{Stdout: []byte("2000\n"), Stderr: []byte("w"), Code: 3}
	})
	cl := serveLocal(t, localExecutor{ex: e.ex, log: discardLogger()})
	var out, errb []byte
	raw, err := cl.Stream(context.Background(), ops.OpAdbShell, ops.AdbShellArgs{Argv: []string{"id -u"}}, func(b json.RawMessage) error {
		var o ops.AdbOutput
		if err := json.Unmarshal(b, &o); err != nil {
			return err
		}
		if o.Stream == "stderr" {
			errb = append(errb, o.Data...)
		} else {
			out = append(out, o.Data...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var res ops.AdbRunResult
	if json.Unmarshal(raw, &res) != nil || res.ExitCode != 3 || string(out) != "2000\n" || string(errb) != "w" {
		t.Fatalf("res %s out %q err %q", raw, out, errb)
	}
	rec := e.ex.DeviceControl().Recent()
	if len(rec) != 1 || rec[0].Session != "build" || rec[0].Op != "shell" || rec[0].Result != "exit:3" {
		t.Fatalf("audit %+v", rec)
	}
}
